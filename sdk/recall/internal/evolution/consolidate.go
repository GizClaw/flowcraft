package evolution

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// Consolidator merges or compacts related facts. Phase 8 ships a
// no-op; real consolidation runs opt-in via EvolutionRunner.
type Consolidator interface {
	Consolidate(ctx context.Context, scope model.Scope) error
}

// NopConsolidator is the default consolidator.
type NopConsolidator struct{}

func (NopConsolidator) Consolidate(context.Context, model.Scope) error { return nil }
