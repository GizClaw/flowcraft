package deploy

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// A session carries the answer and the state that produced it.
func TestSessionRunReportsTheAnswer(t *testing.T) {
	manager := newManager(t, BuiltinKinds().WithEngines(engineFor("echo", func() agent.Runner {
		return &textRunner{text: "hello"}
	})), testHost(t))
	if err := manager.Add(context.Background(), testSpec(t, "a", "echo")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, ok := manager.Session("a", "conv-1")
	if !ok {
		t.Fatal("no handle for a deployed agent")
	}
	if session.ID() != "conv-1" {
		t.Errorf("id = %q", session.ID())
	}
	var text string
	for event, err := range session.Run(context.Background(),
		message.NewTextMessage(message.RoleUser, "hi")) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if typed, ok := event.(agent.Text); ok {
			text += typed.Text
		}
	}
	if text != "hello" {
		t.Errorf("text = %q, want the runner's answer", text)
	}
	if state, err := session.State(context.Background()); err != nil || state != agent.StateCompleted {
		t.Errorf("state = %s (%v), want completed", state, err)
	}
}

// Cancel stops the run in flight and the run records the outcome.
func TestSessionCancelStopsTheRun(t *testing.T) {
	runner := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	manager := newManager(t, BuiltinKinds().WithEngines(engineFor("blocking", func() agent.Runner {
		return runner
	})), testHost(t))
	if err := manager.Add(context.Background(), testSpec(t, "a", "blocking")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, _ := manager.Session("a", "conv-1")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range session.Run(context.Background(), message.NewTextMessage(message.RoleUser, "go")) {
		}
	}()
	<-runner.started
	if err := session.Cancel(context.Background(), "test over"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	<-done
	if !runner.cancelled.Load() {
		t.Error("the run did not observe the cancellation")
	}
	if state, _ := session.State(context.Background()); state != agent.StateCanceled {
		t.Errorf("state = %s, want canceled", state)
	}
	// Cancelling nothing is not an error.
	if err := session.Cancel(context.Background(), "again"); err != nil {
		t.Errorf("second Cancel = %v", err)
	}
}

// A session needs an id before it can carry anything.
func TestSessionRequiresAnID(t *testing.T) {
	manager := newManager(t, BuiltinKinds().WithEngines(engineFor("echo", func() agent.Runner {
		return &textRunner{text: "hello"}
	})), testHost(t))
	if err := manager.Add(context.Background(), testSpec(t, "a", "echo")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, ok := manager.Session("a", "")
	if !ok {
		t.Fatal("no handle for a deployed agent")
	}
	var runErr error
	for _, err := range session.Run(context.Background(),
		message.NewTextMessage(message.RoleUser, "hi")) {
		runErr = err
	}
	if !errors.Is(runErr, agent.ErrSessionRequired) {
		t.Fatalf("run = %v, want ErrSessionRequired", runErr)
	}
}
