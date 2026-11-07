package deploy

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-a2a"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// backend serves one local agent over A2A, so the proxy test talks to a real
// protocol implementation rather than a mock.
func backend(t *testing.T, provider inference.Generator) *httptest.Server {
	t.Helper()
	rt, err := runtime.New(context.Background(), config(t), driver(provider))
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	server, err := a2a.NewServer(a2a.ServerConfig{
		Card: a2a.CardConfig{
			Name: "backend", Version: "1.0.0", URL: "http://backend.invalid",
		},
		Sessions: func(context.Context, *a2asrv.ExecutorContext) (a2a.Runner, error) {
			session, err := agent.NewSession(rt)
			if err != nil {
				return nil, err
			}
			return session, nil
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv := httptest.NewServer(server)
	t.Cleanup(srv.Close)
	return srv
}

// A proxied request travels the whole stack: local proxy → real A2A server →
// local agent → scripted model, and the answer comes back as local events.
func TestProxyForwardsToARemoteAgent(t *testing.T) {
	srv := backend(t, &scripted{steps: []inference.GenerateResponse{textStep("hello from backend")}})
	conversation, err := newProxyConversation(context.Background(),
		EngineSpec{Kind: "a2a", Peer: srv.URL}, nil, "", "conv-1")
	if err != nil {
		t.Fatalf("newProxyConversation: %v", err)
	}

	var text string
	var final agent.TaskState
	for event, err := range conversation.Run(context.Background(), agent.Input{
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
	if text == "" {
		t.Error("the remote answer did not arrive as text events")
	}
	if final != agent.StateCompleted {
		t.Errorf("final state = %s, want completed", final)
	}
	task := conversation.Task()
	if task.ID == "" {
		t.Error("the remote task id was not recorded")
	}
	if task.ContextID != "conv-1" {
		t.Errorf("context = %q, want the local conversation", task.ContextID)
	}
}

// A proxy cannot steer a remote task mid-flight, and says so instead of
// pretending.
func TestProxyDegradesSteerToQueue(t *testing.T) {
	conversation, err := newProxyConversation(context.Background(),
		EngineSpec{Kind: "a2a", Peer: "http://127.0.0.1:1"}, nil, "", "conv-1")
	if err != nil {
		// Connecting is lazy: a client is built from the endpoint without a
		// round trip, so an error here is a real failure.
		t.Fatalf("newProxyConversation: %v", err)
	}
	effective, err := conversation.Send(context.Background(),
		message.NewTextMessage(message.RoleUser, "hi"), agent.ModeSteer)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if effective != agent.ModeQueue {
		t.Errorf("effective mode = %s, want queue", effective)
	}
	if conversation.Pending() != 1 {
		t.Errorf("pending = %d, want 1", conversation.Pending())
	}
}

func TestProxyRequiresAPeer(t *testing.T) {
	if _, err := newProxyConversation(context.Background(), EngineSpec{Kind: "a2a"}, nil, "", "c"); err == nil {
		t.Fatal("a proxy without a peer was accepted")
	}
}
