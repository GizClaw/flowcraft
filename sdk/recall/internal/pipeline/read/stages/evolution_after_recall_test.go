package stages

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/evolution"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/read"
)

// captureRunner is a port.EvolutionRunner stub that records the
// trace it receives so tests can assert exactly what the stage
// surfaced.
type captureRunner struct {
	calls    int
	gotScope domain.Scope
	gotTrace domain.RecallTrace
	err      error
}

func (c *captureRunner) AfterSave(context.Context, domain.Scope, []string) error { return nil }
func (c *captureRunner) AfterRecall(_ context.Context, scope domain.Scope, trace domain.RecallTrace) error {
	c.calls++
	c.gotScope = scope
	c.gotTrace = trace
	return c.err
}

// TestEvolutionAfterRecall_ReadsDropsFromState pins Cluster F
// (2026-05-21): the stage MUST source materialize drops from
// state.MaterializeDrops, NOT state.Trace.Stages. We pre-populate
// drops on State, leave Trace nil, and assert the runner still sees
// those drops (and via the federation_fanout-shaped synthetic stage
// that evolution.PlanFromStages already understands).
func TestEvolutionAfterRecall_ReadsDropsFromState(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	stateDrops := []diagnostic.CandidateDrop{
		{Stage: "materialize", Reason: diagnostic.DropStaleFact, FactID: "f-stale"},
		{Stage: "materialize", Reason: diagnostic.DropSuperseded, FactID: "f-old"},
	}

	state := &read.ReadState{
		Scope:            scope,
		MaterializeDrops: stateDrops,
		// Trace deliberately left nil — the stage MUST NOT reach
		// into it. If it did, the runner would observe an empty
		// trace and PlanFromStages would compute an empty repair
		// plan.
	}

	runner := &captureRunner{}
	stage := NewEvolutionAfterRecall(runner)
	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("AfterRecall calls = %d, want 1", runner.calls)
	}
	if runner.gotScope.CanonicalKey() != scope.CanonicalKey() {
		t.Fatalf("AfterRecall scope = %+v, want %+v", runner.gotScope, scope)
	}
	// The synthetic trace shape MUST be the one
	// evolution.PlanFromStages and diagnostic.ExtractDrops know how
	// to scan — that is the cross-package contract that keeps repair
	// signals flowing even when callers skip RecallExplain.
	got := diagnostic.ExtractDrops(runner.gotTrace.Stages)
	if len(got) != len(stateDrops) {
		t.Fatalf("extracted drops = %d, want %d (%+v)", len(got), len(stateDrops), got)
	}
	for i := range stateDrops {
		if got[i].FactID != stateDrops[i].FactID || got[i].Reason != stateDrops[i].Reason {
			t.Fatalf("drop[%d] = %+v, want %+v", i, got[i], stateDrops[i])
		}
	}

	// And the cross-package repair planner must derive the right
	// fact ids — this is the actual production consumer of the trace
	// the runner receives.
	plan := evolution.PlanFromStages(scope, runner.gotTrace.Stages)
	if len(plan.FactIDs) != 2 {
		t.Fatalf("repair plan FactIDs = %+v, want 2", plan.FactIDs)
	}
}

// TestEvolutionAfterRecall_AggregatesFromSubScopes covers the
// production path where federation_fanout — not the standalone
// Materialize stage — owns drop emission per sub-scope. The stage
// must still surface those drops to the runner without anyone
// touching state.Trace.
func TestEvolutionAfterRecall_AggregatesFromSubScopes(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	state := &read.ReadState{
		Scope: scope,
		SubScopeStates: []read.SubScopeState{
			{
				Scope: scope,
				MaterializeDrops: []diagnostic.CandidateDrop{{
					Stage: "materialize", Reason: diagnostic.DropStaleFact, FactID: "f-a",
				}},
			},
			{
				Scope: scope,
				MaterializeDrops: []diagnostic.CandidateDrop{{
					Stage: "materialize", Reason: diagnostic.DropSuperseded, FactID: "f-b",
				}},
			},
		},
	}

	runner := &captureRunner{}
	if _, err := NewEvolutionAfterRecall(runner).Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := diagnostic.ExtractDrops(runner.gotTrace.Stages)
	if len(got) != 2 {
		t.Fatalf("aggregated drops = %d, want 2 (%+v)", len(got), got)
	}
}

// TestEvolutionAfterRecall_RunnerErrorIsBestEffort confirms that a
// runner failure surfaces as a BestEffort-wrapped error (Cluster C)
// and ALSO populates state.EvolutionErr for back-compat consumers.
func TestEvolutionAfterRecall_RunnerErrorIsBestEffort(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	state := &read.ReadState{
		Scope: scope,
		MaterializeDrops: []diagnostic.CandidateDrop{{
			Stage: "materialize", Reason: diagnostic.DropStaleFact, FactID: "f",
		}},
	}
	boom := errors.New("runner offline")
	runner := &captureRunner{err: boom}
	_, err := NewEvolutionAfterRecall(runner).Run(context.Background(), state)
	if err == nil {
		t.Fatal("Run must return a BestEffort-wrapped error, got nil")
	}
	var bef pipeline.BestEffortFailure
	if !errors.As(err, &bef) {
		t.Fatalf("err must be pipeline.BestEffortFailure, got %T (%v)", err, err)
	}
	if !errors.Is(state.EvolutionErr, boom) {
		t.Fatalf("state.EvolutionErr = %v, want %v", state.EvolutionErr, boom)
	}
}

// TestEvolutionAfterRecall_NilRunnerSkips guards the Conditional
// path so wiring a nil EvolutionRunner does not crash the read
// pipeline (mirrors the production default when WithEvolution is
// not supplied).
func TestEvolutionAfterRecall_NilRunnerSkips(t *testing.T) {
	stage := NewEvolutionAfterRecall(nil)
	skip, _ := stage.Skip(context.Background(), &read.ReadState{})
	if !skip {
		t.Fatal("nil runner must Skip the stage")
	}
}

// statically assert evolution package import is used (linter guard).
var _ = evolution.NopRunner{}
