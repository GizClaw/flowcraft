package maintain

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// stubReclaimer stands in for a projection lane: the pass is what this test
// exercises, not the sweep inside a lane.
type stubReclaimer struct {
	result component.ReclaimResult
	err    error
	calls  int
}

func (stub *stubReclaimer) ReclaimSuperseded(context.Context, corememory.Scope) (component.ReclaimResult, error) {
	stub.calls++
	return stub.result, stub.err
}

func reclaimService(t *testing.T) (*Service, corememory.Scope) {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	kv, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := factview.NewFactStore(logStore, kv)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	return &Service{Facts: facts, Store: store}, corememory.Scope{RuntimeID: "runtime", UserID: "user"}
}

// TestRunScopeReclaimsEveryLanesSupersededObjects pins the pass's second sweep:
// a lane compacts its own storage, and the pass is what collects the
// generations it stopped serving. The count is reported in the plan because it
// describes what the pass removed, not what it judged.
func TestRunScopeReclaimsEveryLanesSupersededObjects(t *testing.T) {
	ctx := context.Background()
	service, scope := reclaimService(t)
	bm25 := &stubReclaimer{result: component.ReclaimResult{Bases: 2, Segments: 3}}
	vector := &stubReclaimer{result: component.ReclaimResult{Bases: 1}}
	service.Projections = []ProjectionReclaimer{
		{Name: "bm25", Reclaimer: bm25},
		// A lane that stores nothing outside its manifest reclaims nothing and
		// is configured without a sweep; the pass must not treat that as an
		// error.
		{Name: "entity"},
		{Name: "vector", Reclaimer: vector},
	}

	plan, err := service.RunScope(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ReclaimedProjectionObjects != 6 {
		t.Fatalf("reclaimed projection objects = %d, want 6", plan.ReclaimedProjectionObjects)
	}
	if bm25.calls != 1 || vector.calls != 1 {
		t.Fatalf("lane sweeps = %d and %d, want one each", bm25.calls, vector.calls)
	}

	// The sweeps are idempotent, so the second pass reclaims what is left --
	// here nothing, which the plan reports as zero and does not omit the rest
	// of the pass over.
	bm25.result, vector.result = component.ReclaimResult{}, component.ReclaimResult{}
	again, err := service.RunScope(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if again.ReclaimedProjectionObjects != 0 {
		t.Fatalf("second pass reclaimed %d objects", again.ReclaimedProjectionObjects)
	}
	if again.ProducerVersion == "" {
		t.Fatalf("second pass reported no stamp: %+v", again)
	}
}

// TestRunScopeReportsTheLaneThatFailedToReclaim pins the failure shape: a sweep
// that fails is returned with the plan, since the overlay it was going to
// report was already saved, and the error names the lane so a host knows which
// projection to look at.
func TestRunScopeReportsTheLaneThatFailedToReclaim(t *testing.T) {
	ctx := context.Background()
	service, scope := reclaimService(t)
	service.Projections = []ProjectionReclaimer{
		{Name: "bm25", Reclaimer: &stubReclaimer{err: errLaneUnavailable{}}},
	}
	plan, err := service.RunScope(ctx, scope)
	if err == nil || !strings.Contains(err.Error(), "bm25") {
		t.Fatalf("pass error = %v, want the lane named", err)
	}
	if plan.ProducerVersion == "" {
		t.Fatalf("a failed sweep did not report the plan it saved: %+v", plan)
	}
}

type errLaneUnavailable struct{}

func (errLaneUnavailable) Error() string { return "lane storage is unavailable" }
