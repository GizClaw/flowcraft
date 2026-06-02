// Package fusion combines multi-source candidate lists into a
// single ranked candidate set. PR-3 ships weighted RRF
// (docs §9.3).
//
// Fusion is deterministic and pure: it must not consult canonical
// store or projection private schema. Stale / superseded filtering
// happens at materialization, not here.
package fusion

import (
	"context"
	"sort"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/evolution"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// Default tuning constants. Callers override per-call via Options.
const (
	DefaultRRFK            = 60
	DefaultRetrievalWeight = 1.0
	DefaultEntityWeight    = 0.8
	DefaultRetrievalFloor  = 5
)

// WeightedRRF implements reciprocal-rank-fusion with per-source
// weights. Same fact id appearing in multiple sources accumulates
// scores; the higher-ranked appearance wins for metadata.
type WeightedRRF struct{}

var _ port.Fuser = WeightedRRF{}

// Fuse runs weighted RRF over results. Returns the fused candidate
// list sorted by score descending, plus structured drops for trace.
func (WeightedRRF) Fuse(_ context.Context, results []domain.SourceResult, opts port.FusionOptions) ([]domain.Candidate, []diagnostic.CandidateDrop, error) {
	if opts.RRFK <= 0 {
		opts.RRFK = DefaultRRFK
	}
	weights := opts.Weights
	if weights == nil {
		weights = map[string]float64{}
	}
	sourceFloors := opts.SourceFloors
	if sourceFloors == nil {
		sourceFloors = map[string]int{"retrieval": DefaultRetrievalFloor}
	}

	var drops []diagnostic.CandidateDrop
	agg := make(map[string]*domain.Candidate)
	order := make([]string, 0)
	floorIDs := map[string]struct{}{}

	for _, res := range results {
		// Truncate each source's contribution to PerSourceCap before
		// fusing, so caps interact with rank instead of post-fusion
		// score (which would be hard to reason about).
		input := res.Candidates
		if opts.PerSourceCap > 0 && len(input) > opts.PerSourceCap {
			for _, c := range input[opts.PerSourceCap:] {
				drops = append(drops, diagnostic.CandidateDrop{
					Stage:  "candidate_merge",
					Reason: diagnostic.DropPerSourceCap,
					FactID: c.ID,
					Source: res.Source,
				})
			}
			input = input[:opts.PerSourceCap]
		}
		if floor := sourceFloors[res.Source]; floor > 0 {
			if floor > len(input) {
				floor = len(input)
			}
			for _, c := range input[:floor] {
				if c.ID != "" {
					floorIDs[c.ID] = struct{}{}
				}
			}
		}
		w := weights[res.Source]
		if w == 0 {
			w = 1.0
		}
		for _, c := range input {
			if c.ID == "" {
				continue
			}
			contribution := w / float64(opts.RRFK+c.Rank)
			contribution *= evolution.FeedbackBoostFromMeta(c.Metadata)
			if existing, ok := agg[c.ID]; ok {
				existing.Score += contribution
				existing.EvidenceIDs = mergeEvidenceIDs(existing.EvidenceIDs, c.EvidenceIDs)
				// Track multi-source membership in metadata so the
				// trace can surface why a fact ranked highly.
				appendSourceMeta(existing, res.Source)
				continue
			}
			merged := c
			merged.Score = contribution
			merged.Metadata = cloneMeta(c.Metadata)
			appendSourceMeta(&merged, res.Source)
			agg[c.ID] = &merged
			order = append(order, c.ID)
		}
	}

	fused := make([]domain.Candidate, 0, len(order))
	for _, id := range order {
		fused = append(fused, *agg[id])
	}
	sort.SliceStable(fused, func(i, j int) bool {
		return fused[i].Score > fused[j].Score
	})

	if opts.TotalCap > 0 && len(fused) > opts.TotalCap {
		kept, dropped := capWithSourceFloors(fused, opts.TotalCap, floorIDs)
		for _, c := range dropped {
			drops = append(drops, diagnostic.CandidateDrop{
				Stage:  "candidate_merge",
				Reason: diagnostic.DropTotalCap,
				FactID: c.ID,
				Source: c.Source,
			})
		}
		fused = kept
	}

	return fused, drops, nil
}

func mergeEvidenceIDs(existing, incoming []string) []string {
	if len(incoming) == 0 {
		return existing
	}
	out := append([]string(nil), existing...)
	seen := make(map[string]struct{}, len(out)+len(incoming))
	for _, id := range out {
		if id != "" {
			seen[id] = struct{}{}
		}
	}
	for _, id := range incoming {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func capWithSourceFloors(sorted []domain.Candidate, totalCap int, floorIDs map[string]struct{}) ([]domain.Candidate, []domain.Candidate) {
	if totalCap <= 0 || len(sorted) <= totalCap {
		return sorted, nil
	}
	if len(floorIDs) == 0 {
		return sorted[:totalCap], sorted[totalCap:]
	}
	keep := make(map[string]struct{}, totalCap)
	for _, c := range sorted {
		if len(keep) >= totalCap {
			break
		}
		if _, ok := floorIDs[c.ID]; !ok {
			continue
		}
		keep[c.ID] = struct{}{}
	}
	for _, c := range sorted {
		if len(keep) >= totalCap {
			break
		}
		if _, ok := keep[c.ID]; ok {
			continue
		}
		keep[c.ID] = struct{}{}
	}
	kept := make([]domain.Candidate, 0, totalCap)
	dropped := make([]domain.Candidate, 0, len(sorted)-len(kept))
	for _, c := range sorted {
		if _, ok := keep[c.ID]; ok {
			kept = append(kept, c)
		} else {
			dropped = append(dropped, c)
		}
	}
	return kept, dropped
}

func appendSourceMeta(c *domain.Candidate, src string) {
	if c.Metadata == nil {
		c.Metadata = map[string]any{}
	}
	existing, _ := c.Metadata["sources"].([]string)
	for _, s := range existing {
		if s == src {
			return
		}
	}
	c.Metadata["sources"] = append(existing, src)
}

func cloneMeta(in map[string]any) map[string]any {
	if len(in) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
