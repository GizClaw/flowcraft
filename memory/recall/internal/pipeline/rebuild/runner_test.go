package rebuild_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/rebuild"
)

// TestRunner_EmptyIsNoOp pins the Phase B.1 contract that an empty
// rebuild pipeline accepts a fresh RebuildState and returns nil
// without mutating anything. Phase B.4 replaces this assertion
// with stage-by-stage coverage.
func TestRunner_EmptyIsNoOp(t *testing.T) {
	r := rebuild.NewRunner(nil, nil)
	state := &rebuild.RebuildState{
		Scope: domain.Scope{RuntimeID: "rt-1"},
	}
	state.EnsureTrace()
	if err := r.Run(context.Background(), state); err != nil {
		t.Fatalf("Run on empty pipeline returned %v, want nil", err)
	}
	if len(state.Trace.Stages) != 0 {
		t.Fatalf("Trace.Stages = %d, want 0 on empty pipeline", len(state.Trace.Stages))
	}
}

// TestRunner_NilSafe documents that a nil receiver is a successful
// no-op.
func TestRunner_NilSafe(t *testing.T) {
	var r *rebuild.Runner
	if err := r.Run(context.Background(), &rebuild.RebuildState{}); err != nil {
		t.Fatalf("nil Runner.Run returned %v, want nil", err)
	}
}

// TestSelectsProjection covers the three branches RebuildAll /
// RebuildProjection / RebuildScope rely on so the project stage
// can route the filter without re-implementing the predicate.
func TestSelectsProjection(t *testing.T) {
	cases := []struct {
		name   string
		filter string
		probe  string
		want   bool
	}{
		{"empty filter selects all", "", "retrieval", true},
		{"exact match", "retrieval", "retrieval", true},
		{"mismatch rejected", "retrieval", "entity", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &rebuild.RebuildState{ProjectionFilter: tc.filter}
			if got := s.SelectsProjection(tc.probe); got != tc.want {
				t.Fatalf("SelectsProjection(%q) with filter=%q = %v, want %v", tc.probe, tc.filter, got, tc.want)
			}
		})
	}
	if (*rebuild.RebuildState)(nil).SelectsProjection("anything") {
		t.Fatal("nil receiver SelectsProjection should be false")
	}
}
