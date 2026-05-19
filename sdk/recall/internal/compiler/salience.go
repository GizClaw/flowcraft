package compiler

import "github.com/GizClaw/flowcraft/sdk/recall/internal/model"

// SalienceScorer assigns confidence / promotion weight to facts. PR-2
// keeps a fixed default so downstream telemetry can rely on a stable
// floor; promotion / decay land in later phases.
type SalienceScorer interface {
	Score(f model.TemporalFact) model.TemporalFact
}

// DefaultConfidence is the floor confidence applied to facts that
// arrive without an explicit score.
const DefaultConfidence = 0.5

type defaultSalienceScorer struct{}

func (defaultSalienceScorer) Score(f model.TemporalFact) model.TemporalFact {
	if f.Confidence == 0 {
		f.Confidence = DefaultConfidence
	}
	return f
}
