package telemetry

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

// TestNopHook_DoesNothing pins the "zero-cost default" contract. The
// fanout falls back to NopHook whenever the caller did not configure
// a real telemetry sink, so it must be safe to call with the zero
// value of every StageDiagnostic field, never panic, and never
// allocate or surface error state to the caller.
func TestNopHook_DoesNothing(t *testing.T) {
	NopHook{}.OnStage(diagnostic.StageDiagnostic{})
	NopHook{}.OnStage(diagnostic.StageDiagnostic{
		Stage:  "validate",
		Phase:  diagnostic.PhaseWrite,
		Status: diagnostic.StatusFailed,
		Err:    "boom",
	})
}
