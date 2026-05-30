package stages

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// fakePlanner counts Plan invocations and captures the last input.
type fakePlanner struct {
	mu         sync.Mutex
	calls      int32
	lastInput  port.PlannerInput
	allInputs  []port.PlannerInput
	planToEmit domain.QueryPlan
}

func (f *fakePlanner) Plan(_ context.Context, in port.PlannerInput) (domain.QueryPlan, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	f.lastInput = in
	f.allInputs = append(f.allInputs, in)
	f.mu.Unlock()
	plan := f.planToEmit
	plan.Intent.Scope = in.Scope
	plan.Intent.Text = in.Text
	plan.Intent.Entities = append([]string(nil), in.Entities...)
	return plan, nil
}

func (f *fakePlanner) Calls() int { return int(atomic.LoadInt32(&f.calls)) }

func newPlanTestState(scope domain.Scope) *read.ReadState {
	state := &read.ReadState{
		Scope: scope,
		Query: domain.Query{Text: "hello"},
	}
	state.Intent = &domain.QueryIntent{
		Text:     "hello",
		Entities: []string{"alice"},
		Scope:    scope,
		Limit:    10,
	}
	return state
}

// TestPlan_SingleScope_RunsPlannerOnce locks that a single-scope Recall makes
// exactly one planner.Plan call.
func TestPlan_SingleScope_RunsPlannerOnce(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	state := newPlanTestState(scope)
	fp := &fakePlanner{planToEmit: domain.QueryPlan{SourceOrder: []string{"retrieval"}, TotalCap: 10}}

	stage := NewPlan(fp, false, func(scopes []domain.Scope) []port.EntitySnapshot {
		if len(scopes) != 1 || scopes[0].CanonicalKey() != scope.CanonicalKey() {
			t.Fatalf("unexpected scopes passed to entitySnapshot: %+v", scopes)
		}
		return nil
	})
	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if got := fp.Calls(); got != 1 {
		t.Fatalf("planner.Plan calls = %d, want 1", got)
	}
	if state.Plan == nil {
		t.Fatal("state.Plan was not populated")
	}
}

// TestPlan_MultiSubScope_RunsPlannerOnce locks the federation case:
// even when the read scope has N sub-scopes the planner is still
// invoked exactly once globally.
func TestPlan_MultiSubScope_RunsPlannerOnce(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	scope.Federation = []domain.Scope{{RuntimeID: "rt"}}
	state := newPlanTestState(scope)
	fp := &fakePlanner{planToEmit: domain.QueryPlan{SourceOrder: []string{"retrieval"}, TotalCap: 10}}

	stage := NewPlan(fp, false, func(scopes []domain.Scope) []port.EntitySnapshot {
		if len(scopes) != 1 {
			t.Fatalf("entitySnapshot called with %d scopes, want exactly 1 per call", len(scopes))
		}
		return nil
	})
	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if got := fp.Calls(); got != 1 {
		t.Fatalf("planner.Plan calls = %d, want 1 across federation", got)
	}
}

// TestPlan_MergesEntitiesAcrossScopes verifies the cross-sub-scope
// EntitySnapshot merge: overlapping canonicals collapse into one entry
// whose Weight reflects the appearance count.
func TestPlan_MergesEntitiesAcrossScopes(t *testing.T) {
	primary := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	sibling := domain.Scope{RuntimeID: "rt"}
	scope := primary
	scope.Federation = []domain.Scope{sibling}
	state := newPlanTestState(scope)

	per := map[string][]port.EntitySnapshot{
		primary.CanonicalKey(): {{Canonical: "Alice"}, {Canonical: "Bob"}},
		sibling.CanonicalKey(): {{Canonical: "alice", Aliases: []string{"Al"}}, {Canonical: "Carol"}},
	}
	fp := &fakePlanner{planToEmit: domain.QueryPlan{SourceOrder: []string{"retrieval"}, TotalCap: 10}}
	stage := NewPlan(fp, false, func(scopes []domain.Scope) []port.EntitySnapshot {
		if len(scopes) != 1 {
			t.Fatalf("entitySnapshot scopes len = %d, want 1", len(scopes))
		}
		return per[scopes[0].CanonicalKey()]
	})
	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if fp.Calls() != 1 {
		t.Fatalf("planner.Plan calls = %d, want 1", fp.Calls())
	}
	got := fp.lastInput.KnownEntities
	if len(got) != 3 {
		t.Fatalf("merged KnownEntities len = %d, want 3 (alice/bob/carol), got %+v", len(got), got)
	}
	byKey := map[string]port.EntitySnapshot{}
	for _, e := range got {
		byKey[canonicalSnapshotKey(e.Canonical)] = e
	}
	alice, ok := byKey["alice"]
	if !ok {
		t.Fatalf("alice missing from merged set: %+v", got)
	}
	if alice.Weight != 2 {
		t.Fatalf("alice merged weight = %v, want 2 (appears in 2 sub-scopes)", alice.Weight)
	}
	if len(alice.Aliases) != 1 || alice.Aliases[0] != "Al" {
		t.Fatalf("alice merged aliases = %+v, want [Al]", alice.Aliases)
	}
	if bob, ok := byKey["bob"]; !ok || bob.Weight != 1 {
		t.Fatalf("bob merged weight = %+v, want Weight=1", bob)
	}
	if carol, ok := byKey["carol"]; !ok || carol.Weight != 1 {
		t.Fatalf("carol merged weight = %+v, want Weight=1", carol)
	}
}

// TestPlan_MergeEntitySnapshots_HonorsRawWeightFloor pins the
// max-with-floor semantic: a pre-set Weight that exceeds the
// appearance count survives the merge so future snapshotters can
// emit their own importance signal.
func TestPlan_MergeEntitySnapshots_HonorsRawWeightFloor(t *testing.T) {
	merged := mergeEntitySnapshots([][]port.EntitySnapshot{
		{{Canonical: "alice", Weight: 5}},
		{{Canonical: "alice"}},
	})
	if len(merged) != 1 {
		t.Fatalf("merged len = %d, want 1", len(merged))
	}
	if merged[0].Weight != 5 {
		t.Fatalf("Weight = %v, want 5 (max(appearances=2, raw=5))", merged[0].Weight)
	}
}
