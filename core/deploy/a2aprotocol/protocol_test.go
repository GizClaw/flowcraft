package a2aprotocol_test

import (
	"context"
	"iter"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	agenta2a "github.com/GizClaw/flowcraft/core/agent-a2a"
	"github.com/GizClaw/flowcraft/core/agent-host"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/deploy/a2aprotocol"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/goccy/go-json"
)

// settingsJSON renders a settings block the way a document is parsed.
func settingsJSON(value any) json.RawMessage {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return body
}

// stubProvider is enough of a driver for a deployment that never calls a model.
type stubProvider struct{}

func (stubProvider) Generate(context.Context, inference.GenerateRequest) (inference.GenerateResponse, error) {
	return inference.GenerateResponse{}, nil
}

func (stubProvider) GenerateStream(context.Context, inference.GenerateRequest) (inference.Stream, error) {
	return nil, nil
}

func stubDriver() runtime.Option {
	return runtime.WithDrivers(runtime.Driver{
		Name: "stub",
		Build: func(json.RawMessage) (inference.Provider, error) {
			return inference.Provider{Generator: stubProvider{}}, nil
		},
	})
}

func runtimeConfig(t *testing.T) runtime.Config {
	t.Helper()
	root := t.TempDir()
	return runtime.Config{
		Version: 1,
		Sandbox: runtime.SandboxConfig{Backend: runtime.BackendLocal, Writes: []string{root}},
		Files:   runtime.FilesConfig{Roots: []string{root}},
		Inference: runtime.InferenceConfig{
			Providers: []runtime.ProviderConfig{{Name: "p", Driver: "stub"}},
			Models:    []runtime.ModelConfig{{Provider: "p", Model: "m"}},
		},
		Tools: runtime.ToolsConfig{Enable: []string{"files"}},
	}
}

func newManager(t *testing.T, kinds deploy.Kinds, bindings host.Bindings) *host.Manager {
	t.Helper()
	manager := host.New(kinds, bindings)
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func specFor(t *testing.T, name, kind string) deploy.Spec {
	t.Helper()
	return deploy.Spec{
		Name:    name,
		Engine:  deploy.EngineSpec{Kind: kind},
		Runtime: runtimeConfig(t),
	}
}

// artifactEngine registers a local engine that produces one artifact.
func artifactEngine() deploy.EngineKind {
	return deploy.EngineKind{
		Name:  "artifact",
		Local: true,
		Build: func(context.Context, deploy.EngineDeps) (agent.RunnerFactory, error) {
			return func(context.Context, string, string) (agent.Runner, error) {
				return artifactRunner{}, nil
			}, nil
		},
	}
}

// textRunner answers with fixed text and finishes.
type textRunner struct{ text string }

func (r *textRunner) State() agent.TaskState { return agent.StateSubmitted }

func (r *textRunner) Run(context.Context, message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		if !yield(agent.Status{State: agent.StateWorking}, nil) {
			return
		}
		if !yield(agent.Text{Text: r.text}, nil) {
			return
		}
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}

func (r *textRunner) Cancel(context.Context, string) error { return nil }
func (r *textRunner) Close() error                         { return nil }

// peer serves an A2A agent answering with text, for tests that need a remote.
func peer(t *testing.T, text string) *httptest.Server {
	t.Helper()
	table, err := agent.NewAgent(agent.AgentOptions{
		NewRunner: func(context.Context, string, string) (agent.Runner, error) {
			return &textRunner{text: text}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = table.Close() })
	srv, err := a2aprotocol.New(a2aprotocol.Config{
		Name: "backend",
		Settings: settingsJSON(map[string]any{"card": map[string]any{
			"version": "1.0.0", "url": "https://backend.test",
		}}),
		Agent: table,
	})
	if err != nil {
		t.Fatalf("a2aprotocol.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// A deployment that names protocol a2a actually serves A2A: the document's
// promise is kept, and a client reaches the agent through the manager's handler.
func TestExposedAgentServesA2A(t *testing.T) {
	peerServer := peer(t, "served")
	manager := newManager(t,
		deploy.BuiltinKinds().WithProtocols(a2aprotocol.Kind()),
		host.Bindings{RuntimeOptions: []runtime.Option{stubDriver()}})
	spec := deploy.Spec{
		Name:   "backend",
		Engine: deploy.EngineSpec{Kind: "a2a", Settings: settingsJSON(map[string]any{"peer": peerServer.URL})},
		Protocol: &deploy.ProtocolSpec{Kind: "a2a", Settings: settingsJSON(map[string]any{
			"card": map[string]any{"version": "1.0.0", "url": "http://backend.invalid"},
		})},
	}
	if err := manager.Add(context.Background(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	handler, ok := manager.Handler("backend")
	if !ok || handler == nil {
		t.Fatal("the agent exposes no handler")
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// The A2A client is the natural test client: it speaks the same protocol.
	client, err := agenta2a.New(context.Background(), agenta2a.Config{
		Peer:      srv.URL,
		SessionID: "conv-1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var text string
	var final agent.TaskState
	for event, err := range client.Run(context.Background(),
		message.NewTextMessage(message.RoleUser, "hi")) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		switch typed := event.(type) {
		case agent.Text:
			text += typed.Text
		case agent.Status:
			final = typed.State
		}
	}
	if text != "served" {
		t.Errorf("text = %q, want the agent's answer", text)
	}
	if final != agent.StateCompleted {
		t.Errorf("final state = %s, want completed", final)
	}
}

// artifactRunner produces one artifact and finishes.
type artifactRunner struct{}

func (artifactRunner) State() agent.TaskState { return agent.StateSubmitted }

func (artifactRunner) Run(context.Context, message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		if !yield(agent.Status{State: agent.StateWorking}, nil) {
			return
		}
		if !yield(agent.ArtifactWritten{Artifact: agent.Artifact{
			ID: "a1", Name: "report", MediaType: "text/plain", Blob: "sha256:1",
		}}, nil) {
			return
		}
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}

func (artifactRunner) Cancel(context.Context, string) error { return nil }
func (artifactRunner) Close() error                         { return nil }

// The deployment decides how a blob becomes fetchable; without the hook the
// server can only send the reference.
func TestArtifactURLReachesTheWire(t *testing.T) {
	kinds := deploy.BuiltinKinds().
		WithEngines(artifactEngine()).
		WithProtocols(a2aprotocol.Kind())
	manager := newManager(t, kinds, host.Bindings{
		RuntimeOptions: []runtime.Option{stubDriver()},
		ArtifactURL:    func(ref string) string { return "https://files.test/" + ref },
	})
	spec := specFor(t, "artifacts", "artifact")
	spec.Protocol = &deploy.ProtocolSpec{Kind: "a2a", Settings: settingsJSON(map[string]any{
		"card": map[string]any{"version": "1.0.0", "url": "http://artifacts.invalid"},
	})}
	if err := manager.Add(context.Background(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	handler, ok := manager.Handler("artifacts")
	if !ok || handler == nil {
		t.Fatal("the agent exposes no handler")
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client, err := agenta2a.New(context.Background(), agenta2a.Config{
		Peer:      srv.URL,
		SessionID: "conv-1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var blob string
	for event, err := range client.Run(context.Background(),
		message.NewTextMessage(message.RoleUser, "make a report")) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if written, ok := event.(agent.ArtifactWritten); ok {
			blob = written.Artifact.Blob
		}
	}
	if blob != "https://files.test/sha256:1" {
		t.Fatalf("artifact blob = %q, want the resolved URL", blob)
	}
}

// Without the protocol registered, the same spec is refused: exposure is a
// deployment choice, not a silent default.
func TestA2AProtocolMustBeRegistered(t *testing.T) {
	manager := newManager(t, deploy.BuiltinKinds(), host.Bindings{})
	err := manager.Add(context.Background(), deploy.Spec{
		Name:     "backend",
		Engine:   deploy.EngineSpec{Kind: "a2a", Settings: settingsJSON(map[string]any{"peer": "https://peer.example"})},
		Protocol: &deploy.ProtocolSpec{Kind: "a2a", Settings: settingsJSON(map[string]any{"card": map[string]any{"version": "1", "url": "https://x"}})},
	})
	if err == nil {
		t.Fatal("an unregistered protocol was accepted")
	}
}

var _ = sync.Mutex{}

// The registration is a value a host composes: a name, a decoder and a builder.
func TestKindRegistersA2A(t *testing.T) {
	kind := a2aprotocol.Kind()
	if kind.Name != "a2a" {
		t.Fatalf("name = %q, want a2a", kind.Name)
	}
	if _, err := kind.Decode(settingsJSON(map[string]any{})); err == nil {
		t.Error("a settings block without a card was accepted")
	}
}
