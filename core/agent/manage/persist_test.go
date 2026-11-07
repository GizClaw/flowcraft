package manage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-a2a"
	"github.com/GizClaw/flowcraft/core/agent/checkpoint"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
)

func openStore(t *testing.T) *checkpoint.SQLite {
	t.Helper()
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "checkpoints.db"))
	if err != nil {
		t.Fatalf("checkpoint.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func runOnce(t *testing.T, conversation *proxyConversation) agent.TaskState {
	t.Helper()
	var final agent.TaskState
	for event, err := range conversation.Run(context.Background(), agent.Input{
		Messages: []message.Message{message.NewTextMessage(message.RoleUser, "hi")},
	}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if status, ok := event.(agent.Status); ok {
			final = status.State
		}
	}
	return final
}

// The remote task identity outlives the process: a new proxy over the same store
// knows which task it was talking to.
func TestProxyRestoresTheRemoteTask(t *testing.T) {
	srv := backend(t, &scripted{steps: []inference.GenerateResponse{textStep("hello")}})
	store := openStore(t)
	digest := runtime.Digest("sha256:deployment")

	first, err := newProxyConversation(context.Background(),
		EngineSpec{Kind: "a2a", Peer: srv.URL}, store, digest, "conv-1")
	if err != nil {
		t.Fatalf("newProxyConversation: %v", err)
	}
	if state := runOnce(t, first); state != agent.StateCompleted {
		t.Fatalf("state = %s", state)
	}
	if first.Task().ID == "" {
		t.Fatal("no remote task id was recorded")
	}

	// A restart: new client, same store.
	second, err := newProxyConversation(context.Background(),
		EngineSpec{Kind: "a2a", Peer: srv.URL}, store, digest, "conv-1")
	if err != nil {
		t.Fatalf("newProxyConversation: %v", err)
	}
	if got := second.Task().ID; got != first.Task().ID {
		t.Errorf("task id = %q, want the restored %q", got, first.Task().ID)
	}
	if got := second.Task().State; got != agent.StateCompleted {
		t.Errorf("state = %s, want completed", got)
	}
}

// A registered protocol uses the deployment's store for task state, so the
// server and the agent read one record instead of two.
func TestExposedAgentUsesTheDeploymentStore(t *testing.T) {
	store := openStore(t)
	manager := New(WithProtocols(A2AProtocol()))
	t.Cleanup(func() { _ = manager.Close() })
	deployed, err := manager.Add(context.Background(), Spec{
		Name:           "backend",
		Engine:         EngineSpec{Kind: "react"},
		Runtime:        config(t),
		RuntimeOptions: []runtime.Option{driver(&scripted{steps: []inference.GenerateResponse{textStep("served")}})},
		Store:          store,
		Protocol: &ProtocolSpec{Kind: "a2a", Decoded: a2a.ServerSettings{
			Card: a2a.CardConfig{Version: "1.0.0", URL: "http://backend.invalid"},
		}},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	srv := httptest.NewServer(deployed.Handler())
	t.Cleanup(srv.Close)
	client, err := newProxyConversation(context.Background(),
		EngineSpec{Kind: "a2a", Peer: srv.URL}, nil, "", "conv-1")
	if err != nil {
		t.Fatalf("newProxyConversation: %v", err)
	}
	runOnce(t, client)

	summaries, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) == 0 {
		t.Error("the server did not persist the task through the deployment store")
	}
}

func TestBearerTokenReadsTheCredential(t *testing.T) {
	if got := bearerToken(map[string]any{"type": "bearer", "token": "abc"}); got != "abc" {
		t.Errorf("token = %q", got)
	}
	if got := bearerToken(map[string]any{"type": "basic", "token": "abc"}); got != "" {
		t.Errorf("an unsupported scheme produced %q", got)
	}
	if got := bearerToken(nil); got != "" {
		t.Errorf("nil auth produced %q", got)
	}
}

func TestBearerTransportAttachesTheHeader(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Transport: bearerTransport{token: "abc", base: http.DefaultTransport}}
	request, err := http.NewRequest(http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = response.Body.Close()
	if seen != "Bearer abc" {
		t.Errorf("authorization = %q", seen)
	}
}
