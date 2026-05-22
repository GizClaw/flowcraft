// Package evolution hosts background memory maintenance hooks
// (docs §10.1). Phase 8 ships no-op defaults that never block
// Save or Recall.
package evolution

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// NopRunner is the default evolution implementation.
type NopRunner struct{}

var _ port.EvolutionRunner = NopRunner{}

func (NopRunner) AfterSave(context.Context, domain.Scope, []string) error { return nil }
func (NopRunner) AfterRecall(context.Context, domain.Scope, domain.RecallTrace) error {
	return nil
}
