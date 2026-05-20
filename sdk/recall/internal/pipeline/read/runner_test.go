package read_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/read"
)

// TestRunner_EmptyIsNoOp pins the Phase B.1 contract that an empty
// read pipeline accepts a fresh ReadState and returns nil without
// mutating anything. Phase B.3 replaces this assertion with
// stage-by-stage coverage.
func TestRunner_EmptyIsNoOp(t *testing.T) {
	r := read.NewRunner(nil, nil)
	state := &read.ReadState{
		Scope: domain.Scope{RuntimeID: "rt-1"},
		Query: domain.Query{Text: "hello"},
	}
	state.EnsureTrace()
	if err := r.Run(context.Background(), state); err != nil {
		t.Fatalf("Run on empty pipeline returned %v, want nil", err)
	}
	if len(state.Trace.Stages) != 0 {
		t.Fatalf("Trace.Stages = %d, want 0 on empty pipeline", len(state.Trace.Stages))
	}
	if state.PrimarySubScope() != nil {
		t.Fatalf("PrimarySubScope() = %v, want nil on empty SubScopeStates", state.PrimarySubScope())
	}
}

// TestRunner_NilSafe documents that a nil receiver is a successful
// no-op.
func TestRunner_NilSafe(t *testing.T) {
	var r *read.Runner
	if err := r.Run(context.Background(), &read.ReadState{}); err != nil {
		t.Fatalf("nil Runner.Run returned %v, want nil", err)
	}
}

// TestReadState_FederationReady locks the per-sub-scope field
// layout in place — this is the structural property that lets D.5
// skip an invasive ReadState refactor. The Phase B.1 commit must
// preserve it; if a future change collapses SubScopeStates this
// test fails loudly.
func TestReadState_FederationReady(t *testing.T) {
	state := &read.ReadState{
		Scope: domain.Scope{RuntimeID: "rt-1", UserID: "alice"},
		SubScopeStates: []read.SubScopeState{
			{Scope: domain.Scope{RuntimeID: "rt-1", UserID: "alice"}, FastPath: true},
		},
	}
	primary := state.PrimarySubScope()
	if primary == nil {
		t.Fatal("PrimarySubScope = nil")
	}
	if !primary.FastPath {
		t.Fatal("expected primary FastPath = true on single-scope reads")
	}
	if primary.Scope.UserID != "alice" {
		t.Fatalf("primary.Scope.UserID = %q, want alice", primary.Scope.UserID)
	}
}
