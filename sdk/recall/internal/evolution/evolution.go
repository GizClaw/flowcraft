// Package evolution hosts background memory maintenance hooks
// (docs §10.1). Phase 8 ships no-op defaults that never block
// Save or Recall.
package evolution

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// Runner observes completed Save/Recall calls. Errors are surfaced
// via telemetry by the Memory facade; they must not fail the
// caller's Save/Recall.
type Runner interface {
	AfterSave(ctx context.Context, scope model.Scope, factIDs []string) error
	AfterRecall(ctx context.Context, scope model.Scope, trace model.RecallTrace) error
}

// NopRunner is the default evolution implementation.
type NopRunner struct{}

func (NopRunner) AfterSave(context.Context, model.Scope, []string) error { return nil }
func (NopRunner) AfterRecall(context.Context, model.Scope, model.RecallTrace) error {
	return nil
}
