package port

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

// EvolutionRunner observes completed Save / Recall calls. Errors
// are surfaced via telemetry by the Memory facade; they must not
// fail the caller's Save / Recall.
type EvolutionRunner interface {
	AfterSave(ctx context.Context, scope domain.Scope, factIDs []string) error
	AfterRecall(ctx context.Context, scope domain.Scope, trace domain.RecallTrace) error
}

// Decayer applies decay / promotion rules to profile slots.
type Decayer interface {
	Apply(ctx context.Context, scope domain.Scope, now time.Time) error
}

// Consolidator merges or compacts related facts.
type Consolidator interface {
	Consolidate(ctx context.Context, scope domain.Scope) error
}
