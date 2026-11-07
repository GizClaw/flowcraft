package deploy

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-a2a"
	"github.com/GizClaw/flowcraft/core/agent/checkpoint"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
)

func waitState(t *testing.T, states chan agent.TaskState, want agent.TaskState) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case state := <-states:
			if state == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", want)
		}
	}
}

// A question asked by a remote agent travels the whole stack: the proxy reports
// input-required, the answer resumes the same remote task, and the pending call
// is what the answer attaches to.
func TestProxyResumeContinuesTheRemoteTask(t *testing.T) {
	manager := New(WithProtocols(A2AProtocol()))
	t.Cleanup(func() { _ = manager.Close() })
	deployed, err := manager.Add(context.Background(), Spec{
		Name:    "backend",
		Engine:  EngineSpec{Kind: "react"},
		Runtime: config(t),
		RuntimeOptions: []runtime.Option{
			driver(pausedProvider(t, "answered remotely")),
		},
		Options: []agent.Option{agent.WithAsk(
			func(context.Context, agent.Prompt) (agent.Reply, error) {
				return agent.Reply{}, agent.ErrPaused
			})},
		Protocol: &ProtocolSpec{Kind: "a2a", Decoded: a2a.ServerSettings{
			Card: a2a.CardConfig{Version: "1.0.0", URL: "http://backend.invalid"},
		}},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	srv := httptest.NewServer(deployed.Handler())
	t.Cleanup(srv.Close)

	proxy, err := newProxyConversation(context.Background(),
		EngineSpec{Kind: "a2a", Peer: srv.URL}, nil, "", "conv-1")
	if err != nil {
		t.Fatalf("newProxyConversation: %v", err)
	}
	var first agent.TaskState
	for event, err := range proxy.Run(context.Background(), agent.Input{
		Messages: []message.Message{message.NewTextMessage(message.RoleUser, "who?")},
	}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if status, ok := event.(agent.Status); ok {
			first = status.State
		}
	}
	if first != agent.StateInputRequired {
		t.Fatalf("state = %s, want input-required", first)
	}
	if proxy.Task().State != agent.StateInputRequired {
		t.Errorf("the proxy did not keep the paused state: %s", proxy.Task().State)
	}

	var final agent.TaskState
	for event, err := range proxy.Resume(context.Background(), agent.Reply{Text: "me"}) {
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if status, ok := event.(agent.Status); ok {
			final = status.State
		}
	}
	if final != agent.StateCompleted {
		t.Errorf("final state = %s, want completed", final)
	}
}

// A paused conversation survives a restart: the new manager restores it from the
// store and the answer continues the same task.
func TestPausedConversationSurvivesARestart(t *testing.T) {
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "checkpoints.db"))
	if err != nil {
		t.Fatalf("checkpoint.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Both managers must build from the same configuration: the roots live in a
	// temp dir that a second config(t) would recreate, changing the digest.
	cfg := config(t)
	first := New()
	states, onEvent := waiting()
	if _, err := first.Add(context.Background(), Spec{
		Name:           "a",
		Engine:         EngineSpec{Kind: "react"},
		Runtime:        cfg,
		RuntimeOptions: []runtime.Option{driver(pausedProvider(t, "answered after restart"))},
		Store:          store,
		Options: []agent.Option{agent.WithAsk(
			func(context.Context, agent.Prompt) (agent.Reply, error) {
				return agent.Reply{}, agent.ErrPaused
			})},
		OnEvent: onEvent,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	deployed, _ := first.Agent("a")
	if err := deployed.Submit(context.Background(), "conv-1",
		message.NewTextMessage(message.RoleUser, "who?"), agent.ModeQueue); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitState(t, states, agent.StateInputRequired)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A new process: same store, same configuration.
	second := New()
	t.Cleanup(func() { _ = second.Close() })
	states2, onEvent2 := waiting()
	if _, err := second.Add(context.Background(), Spec{
		Name:           "a",
		Engine:         EngineSpec{Kind: "react"},
		Runtime:        cfg,
		RuntimeOptions: []runtime.Option{driver(&scripted{steps: []inference.GenerateResponse{textStep("done")}})},
		Store:          store,
		OnEvent:        onEvent2,
	}); err != nil {
		t.Fatalf("Add after restart: %v", err)
	}
	restored, _ := second.Agent("a")
	if err := restored.Resume(context.Background(), "conv-1", agent.Reply{Text: "me"}); err != nil {
		t.Fatalf("Resume after restart: %v", err)
	}
	waitState(t, states2, agent.StateCompleted)
}
