package deploy

import (
	"context"
	"iter"
	"strings"
	"testing"
	"time"

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

// A run-scoped catalog gives every Run its own view.
func TestRunScopedCatalogView(t *testing.T) {
	deployed := &Agent{rt: testCatalogRuntime(t)}
	entry := &conversation{}

	first, ok := runtime.CatalogViewFrom(deployed.runContext(context.Background(), entry))
	if !ok {
		t.Fatal("the run context carries no view")
	}
	second, _ := runtime.CatalogViewFrom(deployed.runContext(context.Background(), entry))
	if first == second {
		t.Fatal("two runs shared one view; discoveries would leak between them")
	}
}

// A conversation-scoped catalog keeps one view per conversation.
func TestConversationScopedCatalogView(t *testing.T) {
	deployed := &Agent{
		rt:   testCatalogRuntime(t),
		spec: Spec{Catalog: CatalogSpec{Scope: CatalogScopeConversation}},
	}
	entry := &conversation{}
	first, ok := runtime.CatalogViewFrom(deployed.runContext(context.Background(), entry))
	if !ok {
		t.Fatal("the run context carries no view")
	}
	second, _ := runtime.CatalogViewFrom(deployed.runContext(context.Background(), entry))
	if first != second {
		t.Fatal("a conversation-scoped view was rebuilt between runs")
	}
	other, _ := runtime.CatalogViewFrom(deployed.runContext(context.Background(), &conversation{}))
	if other == first {
		t.Fatal("two conversations shared one view")
	}
}

// A proxy has no local catalog: the peer decides what the model sees.
func TestProxyHasNoCatalogView(t *testing.T) {
	deployed := &Agent{spec: Spec{Engine: EngineSpec{Kind: "a2a", Peer: "https://peer.example"}}}
	if _, ok := runtime.CatalogViewFrom(deployed.runContext(context.Background(), &conversation{})); ok {
		t.Fatal("a proxy run context carries a local view")
	}
}

// The document can choose the scope, and a typo is a load error.
func TestLoadCatalogScope(t *testing.T) {
	specs, err := loader(t).Load(context.Background(), []byte(`
version: 1
agents:
  - name: translator
    engine: {kind: a2a, peer: https://peer.example}
    catalog: {scope: conversation}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if specs[0].Catalog.Scope != CatalogScopeConversation {
		t.Errorf("scope = %q, want conversation", specs[0].Catalog.Scope)
	}
	_, err = loader(t).Load(context.Background(), []byte(`
version: 1
agents:
  - name: translator
    engine: {kind: a2a, peer: https://peer.example}
    catalog: {scope: forever}
`))
	if err == nil || !strings.Contains(err.Error(), "catalog.scope") {
		t.Fatalf("Load = %v, want a scope complaint", err)
	}
}

// recordingSession captures the context each Run was given.
type recordingSession struct {
	ran chan context.Context
}

func (s *recordingSession) SetIdentity(string, string) {}
func (s *recordingSession) State() agent.TaskState     { return agent.StateSubmitted }

func (s *recordingSession) Run(ctx context.Context, _ agent.Input) iter.Seq2[agent.Event, error] {
	s.ran <- ctx
	return func(yield func(agent.Event, error) bool) {
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}

func (s *recordingSession) Resume(ctx context.Context, _ agent.Reply) iter.Seq2[agent.Event, error] {
	return s.Run(ctx, agent.Input{})
}

func (s *recordingSession) Send(context.Context, message.Message, agent.Mode) (agent.Mode, error) {
	return agent.ModeQueue, nil
}

func (s *recordingSession) Pending() int   { return 0 }
func (s *recordingSession) Abandon() error { return nil }

// The manager hands the view to the engine's Run, not just to a helper.
func TestManagerRunsWithACatalogView(t *testing.T) {
	session := &recordingSession{ran: make(chan context.Context, 4)}
	manager := New(WithEngines(Engine{
		Name: "record",
		Build: func(context.Context, EngineSpec, *Agent) (engine, error) {
			return fakeEngine{session: session}, nil
		},
	}))
	t.Cleanup(func() { _ = manager.Close() })

	deployed, err := manager.Add(context.Background(), Spec{
		Name:    "a",
		Engine:  EngineSpec{Kind: "record"},
		Runtime: config(t),
		RuntimeOptions: []runtime.Option{
			driver(&scripted{}),
		},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := deployed.Submit(context.Background(), "conv-1",
		message.NewTextMessage(message.RoleUser, "go"), agent.ModeQueue); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	select {
	case ctx := <-session.ran:
		if _, ok := runtime.CatalogViewFrom(ctx); !ok {
			t.Fatal("the engine's Run context carries no catalog view")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the session never ran")
	}
}
