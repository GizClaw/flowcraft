package recall

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/evolution"
)

// Reinforce records positive caller feedback on a fact. Subsequent
// recall boosts the fact via fusion and rank using Reinforcement.
func (m *memory) Reinforce(ctx context.Context, scope Scope, factID string, delta float64) error {
	if scope.RuntimeID == "" {
		return errdefs.Validationf("recall.Reinforce: scope.runtime_id is required")
	}
	return evolution.Reinforce(ctx, m.store, scope, factID, delta)
}

// Penalize records negative caller feedback on a fact. Subsequent
// recall down-weights the fact via fusion and rank using Penalty.
func (m *memory) Penalize(ctx context.Context, scope Scope, factID string, delta float64) error {
	if scope.RuntimeID == "" {
		return errdefs.Validationf("recall.Penalize: scope.runtime_id is required")
	}
	return evolution.Penalize(ctx, m.store, scope, factID, delta)
}

func (m *memory) applyFeedback(ctx context.Context, scope Scope, factID string, reinforcementDelta, penaltyDelta float64) error {
	if reinforcementDelta > 0 {
		return evolution.Reinforce(ctx, m.store, scope, factID, reinforcementDelta)
	}
	if penaltyDelta > 0 {
		return evolution.Penalize(ctx, m.store, scope, factID, penaltyDelta)
	}
	return nil
}
