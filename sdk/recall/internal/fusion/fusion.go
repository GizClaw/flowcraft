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
	"math"
	"sort"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// Default tuning constants. Callers override per-call via Options.
const (
	DefaultRRFK            = 60
	DefaultRetrievalWeight = 1.0
	DefaultEntityWeight    = 0.8

	// Outlier-boost defaults. The boost re-introduces score magnitude
	// into RRF for candidates that are clear outliers WITHIN their
	// source — e.g., the only BM25-high hit when the query mentions a
	// rare proper noun. RRF's rank-based aggregation would otherwise
	// allow several mid-rank multi-source hits to outrank a sharp
	// single-source rank-1 outlier; the boost compensates without
	// abandoning RRF's robustness on commensurable-score lanes.
	//
	// Defaults are conservative: a candidate must rank in the top 5
	// of its source AND its score must be >= 2.0x the source's median
	// to qualify, and the boost on its contribution from that source
	// caps at 2.0x. Tune via Options when the source-score scales of
	// your stack make these thresholds wrong.
	DefaultOutlierMaxRank        = 5
	DefaultOutlierScoreThreshold = 2.0
	DefaultOutlierBoostCap       = 2.0
	// Reference window used to compute the source's typical score.
	// 10 keeps the median stable when a single high-BM25 outlier sits
	// above a long tail of low-score candidates (which is exactly the
	// shape we want to detect).
	outlierMedianWindow = 10
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

	// OutlierBoostCap is the maximum multiplier applied to the RRF
	// contribution of a within-source score outlier (see
	// DefaultOutlierBoostCap for rationale). Values <= 1.0 disable the
	// boost. Default DefaultOutlierBoostCap when zero.
	OutlierBoostCap float64
	// OutlierScoreThreshold is the minimum (score / source-median)
	// ratio a candidate must hit to qualify as an outlier. Default
	// DefaultOutlierScoreThreshold when zero.
	OutlierScoreThreshold float64
	// OutlierMaxRank caps how far down the source's ranking we will
	// search for outliers (i.e., only the top N candidates of each
	// source can qualify). Default DefaultOutlierMaxRank when zero.
	OutlierMaxRank int
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
	if opts.OutlierBoostCap == 0 {
		opts.OutlierBoostCap = DefaultOutlierBoostCap
	}
	if opts.OutlierScoreThreshold == 0 {
		opts.OutlierScoreThreshold = DefaultOutlierScoreThreshold
	}
	if opts.OutlierMaxRank == 0 {
		opts.OutlierMaxRank = DefaultOutlierMaxRank
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
		// Precompute the source's reference score so outlier
		// detection runs in O(N log N) per source, not O(N) per
		// candidate. Sources that produce identical scores across
		// candidates (entity / graph / profile when they only signal
		// presence) yield refScore == 0 and effectively disable the
		// boost — exactly the desired behaviour, because there is no
		// score-magnitude signal to amplify.
		refScore := sourceReferenceScore(input)
		for _, c := range input {
			if c.FactID == "" {
				continue
			}
			contribution := w / float64(opts.RRFK+c.Rank)
			if mult := outlierMultiplier(c, refScore, opts); mult > 1 {
				contribution *= mult
			}
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

// sourceReferenceScore returns the median of the top-N candidate
// scores from a single source's result list, used as the denominator
// when measuring per-candidate score outliers. We take the median of
// the head rather than the global median so a long tail of low-score
// candidates (typical for over-fetched BM25 lanes) doesn't pull the
// reference down and turn every candidate into an "outlier". Returns
// 0 when no positive scores are available so the caller can skip the
// boost entirely on sources that signal presence only.
func sourceReferenceScore(in []model.Candidate) float64 {
	if len(in) == 0 {
		return 0
	}
	limit := outlierMedianWindow
	if limit > len(in) {
		limit = len(in)
	}
	head := make([]float64, 0, limit)
	for i := 0; i < limit; i++ {
		if in[i].Score > 0 {
			head = append(head, in[i].Score)
		}
	}
	if len(head) == 0 {
		return 0
	}
	sort.Float64s(head)
	mid := len(head) / 2
	if len(head)%2 == 1 {
		return head[mid]
	}
	return (head[mid-1] + head[mid]) / 2
}

// outlierMultiplier returns the score-magnitude boost a single
// candidate should receive on top of its base RRF contribution. The
// boost only fires when the candidate is a clear within-source
// outlier: its score must exceed OutlierScoreThreshold * refScore AND
// it must rank within the source's top OutlierMaxRank. The
// multiplier scales with log(ratio) so a 10x outlier yields a larger
// boost than a 2x outlier, but the result is capped at
// OutlierBoostCap to prevent any single source from completely
// overriding multi-source corroboration. Returns 1 when no boost
// applies — callers should multiply unconditionally.
func outlierMultiplier(c model.Candidate, refScore float64, opts Options) float64 {
	if refScore <= 0 || c.Score <= 0 || opts.OutlierBoostCap <= 1 {
		return 1
	}
	if c.Rank > opts.OutlierMaxRank {
		return 1
	}
	ratio := c.Score / refScore
	if ratio < opts.OutlierScoreThreshold {
		return 1
	}
	boost := 1 + math.Log(ratio)
	if boost > opts.OutlierBoostCap {
		boost = opts.OutlierBoostCap
	}
	return boost
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
