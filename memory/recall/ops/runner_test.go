package ops

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/store/sqlite"
	recallworkspace "github.com/GizClaw/flowcraft/memory/recall/store/workspace"
)

func TestRunnerDrainScopesProcessesSideEffects(t *testing.T) {
	ctx := context.Background()
	mem, err := recall.New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()

	scope := recall.Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(ctx, scope, recall.SaveRequest{
		Facts: []recall.TemporalFact{{Kind: recall.FactNote, Content: "alpha"}},
	}); err != nil {
		t.Fatal(err)
	}

	var events []Event
	runner, err := NewRunner(mem,
		WithBatchSize(10),
		WithMetrics(MetricsSinkFunc(func(_ context.Context, event Event) {
			events = append(events, event)
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.DrainScopes(ctx, []recall.Scope{scope})
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalClaimed() == 0 || res.Scopes[0].SideEffects.Completed == 0 {
		t.Fatalf("drain result = %+v, want completed side-effect jobs", res)
	}
	if len(events) != 1 || events[0].Kind != EventDrain {
		t.Fatalf("events = %+v, want one drain event", events)
	}
}

func TestRunnerRunDrainsUntilCanceled(t *testing.T) {
	ctx := context.Background()
	mem, err := recall.New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()

	scope := recall.Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(ctx, scope, recall.SaveRequest{
		Facts: []recall.TemporalFact{{Kind: recall.FactNote, Content: "alpha"}},
	}); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	runner, err := NewRunner(mem,
		WithBatchSize(10),
		WithIntervals(time.Millisecond, time.Millisecond),
		WithMetrics(MetricsSinkFunc(func(_ context.Context, event Event) {
			if event.Drain != nil && event.Drain.SideEffects.Completed > 0 {
				cancel()
			}
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	err = runner.Run(runCtx, RunOptions{Target: Target{Scopes: []recall.Scope{scope}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
}

func TestEventSnapshotAndPresets(t *testing.T) {
	local := LocalReadinessOptions()
	if local.MaxSideEffectBacklog == 0 || local.MaxAsyncSemanticBacklog == 0 {
		t.Fatalf("local readiness preset too strict: %+v", local)
	}
	prod := ProductionReadinessOptions()
	if !prod.RequireAsyncSemantic || prod.MaxDeadLetters != 0 {
		t.Fatalf("production readiness preset = %+v, want strict async/dead-letter handling", prod)
	}
	scope := recall.Scope{RuntimeID: "rt", UserID: "u1"}
	event := Event{
		Time:  time.Unix(1, 0),
		Kind:  EventDrain,
		Scope: scope,
		Drain: &ScopeDrainResult{
			Scope: scope,
			SideEffects: recall.SideEffectProcessResult{
				Claimed:   2,
				Completed: 2,
			},
			AsyncSemantic: recall.AsyncSemanticProcessResult{
				Claimed:   1,
				Completed: 1,
				Recovered: 1,
			},
		},
	}
	snap := event.Snapshot()
	if snap.Drain == nil || snap.Drain.Claimed != 3 || snap.Drain.Recovered != 1 {
		t.Fatalf("snapshot = %+v, want combined drain counters", snap)
	}
}

func TestSupervisorStartAndStop(t *testing.T) {
	ctx := context.Background()
	mem, err := recall.New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := recall.Scope{RuntimeID: "rt", UserID: "u1"}
	runner, err := NewRunner(mem, WithIntervals(time.Millisecond, time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := Start(ctx, runner, Target{Scopes: []recall.Scope{scope}})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestRunnerReadinessRuntimeUsesEnumerator(t *testing.T) {
	ctx := context.Background()
	mem, err := recall.New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()

	scopes := []recall.Scope{
		{RuntimeID: "rt", UserID: "u1"},
		{RuntimeID: "rt", UserID: "u2"},
	}
	for _, scope := range scopes {
		if _, err := mem.Save(ctx, scope, recall.SaveRequest{
			Facts: []recall.TemporalFact{{Kind: recall.FactNote, Content: "note"}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	runner, err := NewRunner(mem, WithScopeEnumerator(staticEnumerator(scopes)), WithBatchSize(10))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.DrainRuntime(ctx, "rt"); err != nil {
		t.Fatal(err)
	}
	ready, err := runner.ReadinessRuntime(ctx, "rt")
	if err != nil {
		t.Fatal(err)
	}
	if len(ready.Reports) != 2 {
		t.Fatalf("readiness reports = %d, want 2", len(ready.Reports))
	}
	if ready.Status != recall.ReadinessReady {
		t.Fatalf("readiness status = %s, want ready: %+v", ready.Status, ready)
	}
}

func TestRunnerReconcileRuntimeUsesEnumerator(t *testing.T) {
	ctx := context.Background()
	mem, err := recall.New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()

	scopes := []recall.Scope{
		{RuntimeID: "rt", UserID: "u1"},
		{RuntimeID: "rt", UserID: "u2"},
	}
	for _, scope := range scopes {
		if _, err := mem.Save(ctx, scope, recall.SaveRequest{
			Facts: []recall.TemporalFact{{Kind: recall.FactNote, Content: "note"}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	runner, err := NewRunner(mem, WithScopeEnumerator(staticEnumerator(scopes)))
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.ReconcileRuntime(ctx, "rt")
	if err != nil {
		t.Fatal(err)
	}
	if res.Scopes != 2 || res.Rebuilt != 2 {
		t.Fatalf("reconcile result = %+v, want two rebuilt scopes", res)
	}
}

func TestRunnerDrainsDurableBackendsAfterReopen(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(testing.TB, string) durableParts
	}{
		{
			name: "workspace",
			open: func(t testing.TB, dir string) durableParts {
				t.Helper()
				b, err := recallworkspace.Open(dir)
				if err != nil {
					t.Fatalf("open workspace backend: %v", err)
				}
				store := b.TemporalStore()
				return durableParts{
					store: store,
					side:  b.SideEffectOutbox(),
					async: b.AsyncSemanticQueue(),
				}
			},
		},
		{
			name: "sqlite",
			open: func(t testing.TB, dir string) durableParts {
				t.Helper()
				b, err := sqlite.Open(context.Background(), filepath.Join(dir, "recall.db"))
				if err != nil {
					t.Fatalf("open sqlite backend: %v", err)
				}
				store := b.TemporalStore()
				return durableParts{
					store: store,
					side:  b.SideEffectOutbox(),
					async: b.AsyncSemanticQueue(),
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			scope := recall.Scope{RuntimeID: "rt", UserID: "u1"}
			b := tc.open(t, dir)
			mem, err := recall.New(
				recall.WithTemporalStore(b.store),
				recall.WithSideEffectOutbox(b.side),
				recall.WithAsyncSemanticQueue(b.async),
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := mem.Save(ctx, scope, recall.SaveRequest{
				Facts: []recall.TemporalFact{{Kind: recall.FactNote, Content: "durable"}},
			}); err != nil {
				t.Fatal(err)
			}
			if err := mem.Close(); err != nil {
				t.Fatal(err)
			}

			reopened := tc.open(t, dir)
			mem2, err := recall.New(
				recall.WithTemporalStore(reopened.store),
				recall.WithSideEffectOutbox(reopened.side),
				recall.WithAsyncSemanticQueue(reopened.async),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer mem2.Close()
			runner, err := NewRunner(mem2, WithBatchSize(10), WithClock(func() time.Time {
				return time.Unix(100, 0)
			}))
			if err != nil {
				t.Fatal(err)
			}
			res, err := runner.DrainScopes(ctx, []recall.Scope{scope})
			if err != nil {
				t.Fatal(err)
			}
			if res.TotalClaimed() == 0 || res.Scopes[0].SideEffects.Completed == 0 {
				t.Fatalf("durable drain result = %+v, want completed jobs", res)
			}
		})
	}
}

type durableParts struct {
	store recall.TemporalStore
	side  recall.SideEffectOutbox
	async recall.AsyncSemanticQueue
}

type staticEnumerator []recall.Scope

func (e staticEnumerator) ListScopes(_ context.Context, query recall.ScopeListQuery) ([]recall.Scope, error) {
	var out []recall.Scope
	for _, scope := range e {
		if query.RuntimeID != "" && scope.RuntimeID != query.RuntimeID {
			continue
		}
		out = append(out, recall.Scope{RuntimeID: scope.RuntimeID, UserID: scope.UserID})
	}
	return out, nil
}
