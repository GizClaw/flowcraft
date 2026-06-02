package port

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
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
}

// Fuser combines multi-source candidate streams.
type Fuser interface {
	Fuse(ctx context.Context, results []domain.SourceResult, opts FusionOptions) ([]domain.Candidate, []diagnostic.CandidateDrop, error)
}
