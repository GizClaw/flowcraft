package deploy

import (
	"context"
	"iter"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	agenta2a "github.com/GizClaw/flowcraft/core/agent-a2a"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// A deployment that names protocol a2a actually serves A2A: the document's
// promise is kept, and a client reaches the agent through the manager's handler.
func TestExposedAgentServesA2A(t *testing.T) {
	peer := backend(t, "served")
	manager := newManager(t, BuiltinKinds().WithProtocols(A2AProtocol()), testHost(t))
	spec := Spec{
		Name:   "backend",
		Engine: EngineSpec{Kind: "a2a", Settings: rawSettings(map[string]any{"peer": peer.URL})},
		Protocol: &ProtocolSpec{Kind: "a2a", Settings: rawSettings(map[string]any{
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
	kinds := BuiltinKinds().
		WithEngines(engineFor("artifact", func() agent.Runner { return artifactRunner{} })).
		WithProtocols(A2AProtocol())
	manager := newManager(t, kinds, Host{
		RuntimeOptions: []runtime.Option{driver(&scripted{})},
		ArtifactURL:    func(ref string) string { return "https://files.test/" + ref },
	})
	spec := testSpec(t, "artifacts", "artifact")
	spec.Protocol = &ProtocolSpec{Kind: "a2a", Settings: rawSettings(map[string]any{
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
	manager := newManager(t, BuiltinKinds(), testHost(t))
	err := manager.Add(context.Background(), Spec{
		Name:     "backend",
		Engine:   EngineSpec{Kind: "a2a", Settings: rawSettings(map[string]any{"peer": "https://peer.example"})},
		Protocol: &ProtocolSpec{Kind: "a2a", Settings: rawSettings(map[string]any{"card": map[string]any{"version": "1", "url": "https://x"}})},
	})
	if err == nil {
		t.Fatal("an unregistered protocol was accepted")
	}
}
