package compiler

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// Extractor turns raw input into candidate facts. PR-2 ships only the
// passthrough implementation so callers can begin wiring without an
// LLM dependency; structured / LLM extractors land in Phase 4 under
// the same interface.
type Extractor interface {
	Extract(ctx context.Context, input Input) ([]model.TemporalFact, error)
}

type passthroughExtractor struct{}

// Extract returns the caller-supplied facts unchanged. It deliberately
// ignores Input.Text — text-driven extraction is opt-in and lands in
// a later phase.
func (passthroughExtractor) Extract(_ context.Context, input Input) ([]model.TemporalFact, error) {
	if len(input.Facts) == 0 {
		return nil, nil
	}
	out := make([]model.TemporalFact, len(input.Facts))
	for i, f := range input.Facts {
		out[i] = f.Clone()
	}
	return out, nil
}
