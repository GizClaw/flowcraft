package read

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

// PublicRecallTrace materialises domain.RecallTrace from ReadState.
// The public surface is Stages-only; callers reconstruct any derived
// view via sdk/recall/diagnostics.
func PublicRecallTrace(state *ReadState) domain.RecallTrace {
	if state == nil || state.Trace == nil {
		return domain.RecallTrace{}
	}
	return domain.RecallTrace{
		Stages: append([]diagnostic.StageDiagnostic(nil), state.Trace.Stages...),
	}
}
