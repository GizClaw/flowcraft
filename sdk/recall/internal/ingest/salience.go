package ingest

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// DefaultConfidence is the floor confidence applied to facts that
// arrive without an explicit score.
const DefaultConfidence = 0.5

type defaultSalienceScorer struct{}

var _ port.SalienceScorer = defaultSalienceScorer{}

func (defaultSalienceScorer) Score(f domain.TemporalFact) domain.TemporalFact {
	if f.Confidence == 0 {
		f.Confidence = DefaultConfidence
	}
	return f
}
