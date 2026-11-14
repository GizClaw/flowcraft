package host

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/message"
)

// A session carries the answer and the state that produced it.
func TestSessionRunReportsTheAnswer(t *testing.T) {
	manager := newManager(t, deploy.BuiltinKinds().WithEngines(engineFor("echo", func() agent.Runner {
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
	manager := newManager(t, deploy.BuiltinKinds().WithEngines(engineFor("blocking", func() agent.Runner {
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
	manager := newManager(t, deploy.BuiltinKinds().WithEngines(engineFor("echo", func() agent.Runner {
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

// A host that serves one tenant stamps it on every finished run, so an OnRun
// consumer can bucket records without a side channel.
func TestSessionRunCarriesTheTenant(t *testing.T) {
	var records []agent.RunRecord
	host := testHost(t)
	host.TenantID = "acme"
	host.OnRun = func(_ string, record agent.RunRecord) {
		records = append(records, record)
	}
	manager := newManager(t, deploy.BuiltinKinds().WithEngines(engineFor("echo", func() agent.Runner {
		return &textRunner{text: "hello"}
	})), host)
	if err := manager.Add(context.Background(), testSpec(t, "a", "echo")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, _ := manager.Session("a", "conv-1")
	for range session.Run(context.Background(), message.NewTextMessage(message.RoleUser, "hi")) {
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want one", len(records))
	}
	if records[0].TenantID != "acme" {
		t.Fatalf("tenant = %q, want acme", records[0].TenantID)
	}
}

type resultTextRunner struct {
	*textRunner
	content message.Content
}

func (r *resultTextRunner) Result(context.Context) (agent.Result, error) {
	return agent.Result{State: agent.StateCompleted, Content: r.content}, nil
}

// Session.Result forwards to the agent and returns only the final assistant
// message.
func TestSessionResultReturnsTheFinalAnswer(t *testing.T) {
	manager := newManager(t, deploy.BuiltinKinds().WithEngines(engineFor("result", func() agent.Runner {
		return &resultTextRunner{
			textRunner: &textRunner{text: "hello"},
			content:    message.NewTextContent("hello"),
		}
	})), testHost(t))
	if err := manager.Add(context.Background(), testSpec(t, "a", "result")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, _ := manager.Session("a", "conv-1")
	for range session.Run(context.Background(), message.NewTextMessage(message.RoleUser, "hi")) {
	}
	result, err := session.Result(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if result.TaskID == "" {
		t.Error("task id is empty")
	}
	if result.Content.Text() != "hello" {
		t.Errorf("content = %q", result.Content.Text())
	}
}
