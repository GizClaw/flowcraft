package deploy

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-a2a"
	"github.com/GizClaw/flowcraft/core/message"
)

// A deployment that declares protocol a2a actually serves A2A: the config's
// promise is kept, and a client reaches the agent through the handler the
// manager built.
func TestExposedAgentServesA2A(t *testing.T) {
	manager := New(WithProtocols(A2AProtocol()))
	t.Cleanup(func() { _ = manager.Close() })

	peer := backend(t, "served")
	deployed, err := manager.Add(context.Background(), Spec{
		Name:   "backend",
		Engine: EngineSpec{Kind: "a2a", Peer: peer.URL},
		Protocol: &ProtocolSpec{
			Kind: "a2a",
			Decoded: a2a.ServerSettings{Card: a2a.CardConfig{
				Version: "1.0.0", URL: "http://backend.invalid",
			}},
		},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	handler := deployed.Handler()
	if handler == nil {
		t.Fatal("the agent exposes no handler")
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// The proxy is the only A2A client in the tree that needs no extra wiring,
	// so it doubles as the test client.
	client, err := newProxyConversation(context.Background(), EngineSpec{Kind: "a2a", Peer: srv.URL}, nil, "", "conv-1")
	if err != nil {
		t.Fatalf("newProxyConversation: %v", err)
	}
	var text string
	var final agent.TaskState
	for event, err := range client.Run(context.Background(), agent.Input{
		Messages: []message.Message{message.NewTextMessage(message.RoleUser, "hi")},
	}) {
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

// Without the protocol registered, the same spec is refused: exposure is a
// deployment choice, not a silent default.
func TestA2AProtocolMustBeRegistered(t *testing.T) {
	manager := New()
	t.Cleanup(func() { _ = manager.Close() })
	_, err := manager.Add(context.Background(), Spec{
		Name:     "backend",
		Engine:   EngineSpec{Kind: "a2a", Peer: "https://peer.example/.well-known/agent-card.json"},
		Protocol: &ProtocolSpec{Kind: "a2a"},
	})
	if err == nil {
		t.Fatal("an unregistered protocol was accepted")
	}
}
