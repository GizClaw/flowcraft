package ingest

import (
	"math"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// DefaultConfidence is the floor confidence applied to facts that
// arrive without an explicit score.
const DefaultConfidence = 0.5

// tierConfidenceDelta maps SaveRequest.Tier intent labels to
// confidence adjustments (Phase D.3). Caller-supplied Confidence on
// a fact is preserved as the base before the tier delta is applied.
var tierConfidenceDelta = map[string]float64{
	domain.TierCore:    0.3,
	domain.TierGeneral: 0,
	domain.TierData:    -0.1,
	domain.TierStorage: -0.3,
}

type defaultSalienceScorer struct {
	tier string
}

var _ port.SalienceScorer = defaultSalienceScorer{}

// Score applies default confidence and optional SaveRequest.Tier
// adjustment. Tier is an intent label at save time, not a persisted
// fact attribute — callers needing finer gradients should set
// Confidence directly on each TemporalFact.
func (s defaultSalienceScorer) Score(f domain.TemporalFact) domain.TemporalFact {
	base := f.Confidence
	if base == 0 {
		base = DefaultConfidence
	}
	tier := domain.NormalizeSaveTier(s.tier)
	if delta, ok := tierConfidenceDelta[tier]; ok {
		base += delta
	}
	f.Confidence = clamp01(base)
	return f
}

func clamp01(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}

// TierAppliedFor returns the normalized tier label recorded in
// IngestDetail.TierApplied.
func TierAppliedFor(tier string) string {
	return domain.NormalizeSaveTier(tier)
}
