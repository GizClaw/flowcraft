package port

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

// FusionOptions controls a single Fuse call. Zero values fall back
// to the implementation defaults.
type FusionOptions struct {
	// Weights maps source name -> RRF weight. Missing names default
	// to 1.0.
	Weights map[string]float64
	// RRFK is the standard RRF denominator constant.
	RRFK int
	// PerSourceCap caps each source's contribution AFTER ranking.
	// 0 = unlimited.
	PerSourceCap int
	// TotalCap is the upper bound on the returned candidate slice.
	// 0 = unlimited.
	TotalCap int
	// SourceFloors guarantees the top N candidates from a source remain
	// eligible after total-cap truncation. It prevents high-confidence
	// single-source evidence from being completely displaced by
	// multi-source RRF corroboration. Missing names default to no floor.
	SourceFloors map[string]int
	// OutlierBoostCap caps the multiplier applied to within-source
	// score outliers. Values <= 1.0 disable the boost.
	OutlierBoostCap float64
	// OutlierScoreThreshold is the minimum (score / source-median)
	// ratio a candidate must hit to qualify as an outlier.
	OutlierScoreThreshold float64
	// OutlierMaxRank caps how far down the source's ranking we
	// will search for outliers.
	OutlierMaxRank int
}

// Fuser combines multi-source candidate streams.
type Fuser interface {
	Fuse(ctx context.Context, results []domain.SourceResult, opts FusionOptions) ([]domain.Candidate, []diagnostic.CandidateDrop, error)
}
