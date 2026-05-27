package write_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
)

// TestRunner_EmptyIsNoOp pins the contract that an empty write
// pipeline accepts a fresh WriteState and returns nil without
// mutating anything.
func TestRunner_EmptyIsNoOp(t *testing.T) {
	r := write.NewRunner(nil, nil)
	state := &write.WriteState{
		Scope: domain.Scope{RuntimeID: "rt-1"},
	}
	state.EnsureTrace()
	if err := r.Run(context.Background(), state); err != nil {
		t.Fatalf("Run on empty pipeline returned %v, want nil", err)
	}
	if len(state.Trace.Stages) != 0 {
		t.Fatalf("Trace.Stages = %d, want 0 on empty pipeline", len(state.Trace.Stages))
	}
	if state.HasWork() {
		t.Fatalf("HasWork() = true on a state with no resolution facts")
	}
}

// TestRunner_NilSafe documents that a nil receiver is a successful
// no-op — convenient for tests that thread a struct field through.
func TestRunner_NilSafe(t *testing.T) {
	var r *write.Runner
	if err := r.Run(context.Background(), &write.WriteState{}); err != nil {
		t.Fatalf("nil Runner.Run returned %v, want nil", err)
	}
}
