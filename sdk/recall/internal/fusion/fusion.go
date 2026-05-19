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

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// Default tuning constants. Callers override per-call via Options.
const (
	DefaultRRFK            = 60
	DefaultRetrievalWeight = 1.0
	DefaultEntityWeight    = 0.8
)

// Options controls a single Fuse call. Zero values fall back to
// PR-3 defaults so callers can pass Options{} when defaults are
// fine.
type Options struct {
	// Weights maps source name -> RRF weight. Missing names default
	// to 1.0.
	Weights map[string]float64
	// RRFK is the standard RRF denominator constant. Defaults to
	// DefaultRRFK (60) which is the conventional value.
	RRFK int
	// PerSourceCap caps each source's contribution AFTER ranking.
	// 0 = unlimited.
	PerSourceCap int
	// TotalCap is the upper bound on the returned candidate slice.
	// 0 = unlimited.
	TotalCap int
}

// Fuser combines multi-source candidate streams. PR-3 ships only
// WeightedRRF; alternate fusers (linear combination, learned-rank)
// can plug behind this interface in later phases.
type Fuser interface {
	Fuse(ctx context.Context, results []model.SourceResult, opts Options) ([]model.Candidate, []model.CandidateDrop, error)
}

// WeightedRRF implements reciprocal-rank-fusion with per-source
// weights. Same fact id appearing in multiple sources accumulates
// scores; the higher-ranked appearance wins for metadata.
type WeightedRRF struct{}

// Fuse runs weighted RRF over results. Returns the fused candidate
// list sorted by score descending, plus structured drops for trace.
func (WeightedRRF) Fuse(_ context.Context, results []model.SourceResult, opts Options) ([]model.Candidate, []model.CandidateDrop, error) {
	if opts.RRFK <= 0 {
		opts.RRFK = DefaultRRFK
	}
	weights := opts.Weights
	if weights == nil {
		weights = map[string]float64{}
	}

	var drops []model.CandidateDrop
	agg := make(map[string]*model.Candidate)
	order := make([]string, 0)

	for _, res := range results {
		// Truncate each source's contribution to PerSourceCap before
		// fusing, so caps interact with rank instead of post-fusion
		// score (which would be hard to reason about).
		input := res.Candidates
		if opts.PerSourceCap > 0 && len(input) > opts.PerSourceCap {
			for _, c := range input[opts.PerSourceCap:] {
				drops = append(drops, model.CandidateDrop{
					Stage:  "fusion",
					Reason: model.DropPerSourceCap,
					FactID: c.FactID,
					Source: res.Source,
				})
			}
			input = input[:opts.PerSourceCap]
		}
		w := weights[res.Source]
		if w == 0 {
			w = 1.0
		}
		for _, c := range input {
			if c.FactID == "" {
				continue
			}
			contribution := w / float64(opts.RRFK+c.Rank)
			if existing, ok := agg[c.FactID]; ok {
				existing.Score += contribution
				// Track multi-source membership in metadata so the
				// trace can surface why a fact ranked highly.
				appendSourceMeta(existing, res.Source)
				continue
			}
			merged := c
			merged.Score = contribution
			merged.Metadata = cloneMeta(c.Metadata)
			appendSourceMeta(&merged, res.Source)
			agg[c.FactID] = &merged
			order = append(order, c.FactID)
		}
	}

	fused := make([]model.Candidate, 0, len(order))
	for _, id := range order {
		fused = append(fused, *agg[id])
	}
	sort.SliceStable(fused, func(i, j int) bool {
		return fused[i].Score > fused[j].Score
	})

	if opts.TotalCap > 0 && len(fused) > opts.TotalCap {
		for _, c := range fused[opts.TotalCap:] {
			drops = append(drops, model.CandidateDrop{
				Stage:  "fusion",
				Reason: model.DropTotalCap,
				FactID: c.FactID,
				Source: c.Source,
			})
		}
		fused = fused[:opts.TotalCap]
	}

	return fused, drops, nil
}

func appendSourceMeta(c *model.Candidate, src string) {
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
