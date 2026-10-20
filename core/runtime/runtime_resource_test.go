package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
)

const testDBResourceKind = "runtime-test-db"

type testDB struct {
	name string
}

func TestRuntimeResourceReturnsBuiltValue(t *testing.T) {
	db := &testDB{name: "pool-a"}
	reg := newBaseRegistry(t, event.NewMemoryBus(), &recordingCheckpointStore{}, noopEngine())
	resource.MustRegister(reg, testResourceFactory{
		spec:  resource.Spec{Kind: testDBResourceKind, Impl: "a"},
		value: db,
	})

	doc := baseRuntimeDoc(t)
	doc.Resources["db"] = resource.Resource{Kind: testDBResourceKind, Impl: "a"}

	app, err := NewBuilder(reg).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	got, ok := app.Resource("db")
	if !ok || got != db {
		t.Fatalf("Resource(db) = (%v, %v), want (%v, true)", got, ok, db)
	}
	if got, ok := app.Resource("missing"); ok || got != nil {
		t.Fatalf("Resource(missing) = (%v, %v), want (nil, false)", got, ok)
	}
	var nilApp *Runtime
	if got, ok := nilApp.Resource("db"); ok || got != nil {
		t.Fatalf("nil Runtime Resource(db) = (%v, %v), want (nil, false)", got, ok)
	}
}

func TestRuntimeResourceFollowsReloadGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	dbA := &testDB{name: "pool-a"}
	dbB := &testDB{name: "pool-b"}
	reg := resource.NewRegistry()
	resource.MustRegister(reg, testEngineFactory{engine: noopEngine()})
	resource.MustRegister(reg, event.NewFactory())
	resource.MustRegister(reg, testResourceFactory{
		spec:  resource.Spec{Kind: testDBResourceKind, Impl: "a"},
		value: dbA,
	})
	resource.MustRegister(reg, testResourceFactory{
		spec:  resource.Spec{Kind: testDBResourceKind, Impl: "b"},
		value: dbB,
	})

	agents := `  bot:
    card: {name: Bot}
    engine: {kind: agent.Engine, impl: test}
`
	app, err := NewBuilder(reg).Build(ctx, reloadDoc(t, agents, `  db: {kind: runtime-test-db, impl: a}
`))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	got, ok := app.Resource("db")
	if !ok || got != dbA {
		t.Fatalf("Resource(db) before reload = (%v, %v), want (%v, true)", got, ok, dbA)
	}

	if _, err := app.Reload(ctx, reloadDoc(t, agents, `  db: {kind: runtime-test-db, impl: b}
`)); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	got, ok = app.Resource("db")
	if !ok || got != dbB {
		t.Fatalf("Resource(db) after reload = (%v, %v), want (%v, true)", got, ok, dbB)
	}
	if got, ok := app.Resource("missing"); ok || got != nil {
		t.Fatalf("Resource(missing) after reload = (%v, %v), want (nil, false)", got, ok)
	}
}

// TestRuntimeResourceConcurrentReload exercises the reader side of a
// reload: Resource resolves through the current generation without
// taking the lifecycle lock, so it must be safe to call while Reload
// swaps that generation. Run with -race (CI does), this is the
// regression test for reading the generation pointer without
// synchronisation; without the race detector it still pins the
// contract that a reader sees one complete generation's value — never
// a missing or half-swapped one.
func TestRuntimeResourceConcurrentReload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbA := &testDB{name: "pool-a"}
	dbB := &testDB{name: "pool-b"}
	reg := resource.NewRegistry()
	resource.MustRegister(reg, testEngineFactory{engine: noopEngine()})
	resource.MustRegister(reg, event.NewFactory())
	resource.MustRegister(reg, testResourceFactory{
		spec:  resource.Spec{Kind: testDBResourceKind, Impl: "a"},
		value: dbA,
	})
	resource.MustRegister(reg, testResourceFactory{
		spec:  resource.Spec{Kind: testDBResourceKind, Impl: "b"},
		value: dbB,
	})

	agents := `  bot:
    card: {name: Bot}
    engine: {kind: agent.Engine, impl: test}
`
	app, err := NewBuilder(reg).Build(ctx, reloadDoc(t, agents, `  db: {kind: runtime-test-db, impl: a}
`))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, ok := app.Resource("db")
				if !ok {
					t.Errorf("Resource(db) missed during reload")
					return
				}
				if got != dbA && got != dbB {
					t.Errorf("Resource(db) = %v, want one generation's value", got)
					return
				}
			}
		}()
	}

	for i := range 5 {
		impl := "a"
		if i%2 == 1 {
			impl = "b"
		}
		if _, err := app.Reload(ctx, reloadDoc(t, agents,
			"  db: {kind: runtime-test-db, impl: "+impl+"}\n")); err != nil {
			t.Fatalf("Reload %d: %v", i, err)
		}
	}

	close(stop)
	wg.Wait()
}
