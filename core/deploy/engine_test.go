package deploy

import (
	"context"
	"encoding/json"
	"iter"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	agenta2a "github.com/GizClaw/flowcraft/core/agent-a2a"
	"github.com/GizClaw/flowcraft/core/agent-a2a/server"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// An auth block this build cannot honour is refused instead of being dropped:
// connecting anonymously would look like a peer outage.
func TestA2AEngineRejectsUnsupportedAuth(t *testing.T) {
	manager := New()
	t.Cleanup(func() { _ = manager.Close() })
	for name, auth := range map[string]json.RawMessage{
		"type":  json.RawMessage(`{"type":"basic","token":"secret"}`),
		"empty": json.RawMessage(`{"type":"bearer"}`),
		"junk":  json.RawMessage(`"not an object"`),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := manager.Add(context.Background(), Spec{
				Name:   "peer-" + name,
				Engine: EngineSpec{Kind: "a2a", Peer: "https://peer.test", Auth: auth},
			})
			if err == nil || !strings.Contains(err.Error(), "a2a auth") {
				t.Fatalf("Add = %v, want an auth complaint", err)
			}
		})
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

// The deployment decides how a blob becomes fetchable; without the field the
// server can only send the reference.
func TestArtifactURLReachesTheWire(t *testing.T) {
	manager := New(
		WithEngines(Engine{
			Name:  "artifact",
			Local: true,
			Build: func(context.Context, EngineDeps) (RunnerFactory, error) {
				return func(context.Context, string, string) (agent.Runner, error) {
					return artifactRunner{}, nil
				}, nil
			},
		}),
		WithProtocols(A2AProtocol()),
	)
	t.Cleanup(func() { _ = manager.Close() })
	deployed, err := manager.Add(context.Background(), Spec{
		Name:    "artifacts",
		Engine:  EngineSpec{Kind: "artifact"},
		Runtime: config(t),
		RuntimeOptions: []runtime.Option{
			driver(&scripted{}),
		},
		Protocol: &ProtocolSpec{
			Kind: "a2a",
			Decoded: server.ServerSettings{Card: server.CardConfig{
				Version: "1.0.0", URL: "http://artifacts.invalid",
			}},
		},
		ArtifactURL: func(ref string) string { return "https://files.test/" + ref },
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	http := httptest.NewServer(deployed.Handler())
	t.Cleanup(http.Close)
	client, err := agenta2a.New(context.Background(), agenta2a.Config{
		Peer:           http.URL,
		ConversationID: "conv-1",
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
