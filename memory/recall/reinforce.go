package recall

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/feedback"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Reinforce records positive caller feedback on a fact via the feedback
// pipeline.
func (m *memory) Reinforce(ctx context.Context, scope Scope, factID string, delta float64) error {
	return m.runFeedback(ctx, scope, &feedback.State{Scope: scope, FactID: factID, ReinforcementDelta: delta})
}

// Penalize records negative caller feedback on a fact.
func (m *memory) Penalize(ctx context.Context, scope Scope, factID string, delta float64) error {
	return m.runFeedback(ctx, scope, &feedback.State{Scope: scope, FactID: factID, PenaltyDelta: delta})
}

func (m *memory) runFeedback(ctx context.Context, scope Scope, st *feedback.State) error {
	if scope.RuntimeID == "" {
		return errdefs.Validationf("recall.Feedback: scope.runtime_id is required")
	}
	m.holdWriteTelemetry()
	unlock := m.lockWriteScope(scope)
	defer func() {
		unlock()
		m.flushWriteTelemetry()
	}()
	return m.feedbackRunner.Run(ctx, st)
}
