package deploy

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
)

func testCatalogRuntime(t *testing.T) *runtime.Runtime {
	t.Helper()
	rt, err := runtime.New(context.Background(), config(t), driver(&scripted{}))
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func catalogView(t *testing.T, d *deployment, sessionID string) any {
	t.Helper()
	view, ok := runtime.CatalogViewFrom(d.runContext(context.Background(), sessionID))
	if !ok {
		t.Fatal("the run context carries no view")
	}
	return view
}

// A run-scoped catalog gives every Run its own view.
func TestRunScopedCatalogView(t *testing.T) {
	d := &deployment{rt: testCatalogRuntime(t)}
	first := catalogView(t, d, "conv-1")
	second := catalogView(t, d, "conv-1")
	if first == second {
		t.Fatal("two runs shared one view; discoveries would leak between them")
	}
}

// A session-scoped catalog keeps one view per session.
func TestSessionScopedCatalogView(t *testing.T) {
	d := &deployment{
		rt:   testCatalogRuntime(t),
		spec: Spec{Catalog: CatalogSpec{Scope: CatalogScopeSession}},
	}
	first := catalogView(t, d, "conv-1")
	second := catalogView(t, d, "conv-1")
	if first != second {
		t.Fatal("a session-scoped view was rebuilt between runs")
	}
	other := catalogView(t, d, "conv-2")
	if other == first {
		t.Fatal("two sessions shared one view")
	}
}

// A remote engine has no local catalog: the peer decides what the model sees.
func TestRemoteEngineHasNoCatalogView(t *testing.T) {
	d := &deployment{spec: Spec{Engine: EngineSpec{Kind: "a2a"}}}
	if _, ok := runtime.CatalogViewFrom(d.runContext(context.Background(), "conv-1")); ok {
		t.Fatal("a remote run context carries a local view")
	}
}

// The view lives exactly as long as its session: the table says when it
// ends, and deploy drops the view with it.
func TestSessionScopedViewIsDroppedOnEviction(t *testing.T) {
	manager := newManager(t, BuiltinKinds().WithEngines(engineFor("done", func() agent.Runner {
		return &completedRunner{}
	})), testHost(t))
	spec := testSpec(t, "a", "done")
	spec.Catalog = CatalogSpec{Scope: CatalogScopeSession}
	spec.MaxSessions = 1
	if err := manager.Add(context.Background(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	deployed := manager.agents["a"]
	first, _ := manager.Session("a", "conv-1")
	for range first.Run(context.Background(), message.NewTextMessage(message.RoleUser, "go")) {
	}
	if _, ok := deployed.views.Load("conv-1"); !ok {
		t.Fatal("the run cached no view")
	}
	second, _ := manager.Session("a", "conv-2")
	for range second.Run(context.Background(), message.NewTextMessage(message.RoleUser, "go")) {
	}
	if _, ok := deployed.views.Load("conv-1"); ok {
		t.Fatal("the view outlived its session")
	}
}

// The document can choose the scope, and a typo is a load error.
func TestLoadCatalogScope(t *testing.T) {
	loader := testLoader(t)
	if _, err := loader.Load(context.Background(), []byte(`
version: 1
agents:
  - name: translator
    engine: {kind: a2a, settings: {peer: https://peer.example}}
    catalog: {scope: session}
`)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err := loader.Load(context.Background(), []byte(`
version: 1
agents:
  - name: translator
    engine: {kind: a2a, settings: {peer: https://peer.example}}
    catalog: {scope: forever}
`))
	if err == nil {
		t.Fatal("an unsupported scope was accepted")
	}
}
