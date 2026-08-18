package runtime

import (
	"context"
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
	reg.MustRegister(testResourceFactory{
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
	reg.MustRegister(testEngineFactory{engine: noopEngine()})
	reg.MustRegister(event.NewFactory())
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: testDBResourceKind, Impl: "a"},
		value: dbA,
	})
	reg.MustRegister(testResourceFactory{
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
