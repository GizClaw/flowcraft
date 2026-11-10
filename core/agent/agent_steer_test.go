package agent_test

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// steerRunner is a running engine that takes steers, plus a plain variant for
// engines that cannot.
type steerRunner struct {
	mu       sync.Mutex
	started  chan struct{}
	release  chan struct{}
	steers   []message.Message
	steerErr error
}

func (r *steerRunner) State() agent.TaskState { return agent.StateWorking }

func (r *steerRunner) Run(ctx context.Context, _ message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		if !yield(agent.Status{State: agent.StateWorking}, nil) {
			return
		}
		close(r.started)
		select {
		case <-ctx.Done():
			return
		case <-r.release:
		}
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}

func (r *steerRunner) Steer(_ context.Context, msg message.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.steerErr != nil {
		return r.steerErr
	}
	r.steers = append(r.steers, msg)
	return nil
}

func (r *steerRunner) Cancel(context.Context, string) error { return nil }
func (r *steerRunner) Close() error                         { return nil }

// plainRunner runs but has no Steer method, so the capability is absent.
type plainRunner struct {
	started chan struct{}
	release chan struct{}
}

func (r *plainRunner) State() agent.TaskState { return agent.StateWorking }
func (r *plainRunner) Run(context.Context, message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		if !yield(agent.Status{State: agent.StateWorking}, nil) {
			return
		}
		close(r.started)
		<-r.release
	}
}
func (r *plainRunner) Cancel(context.Context, string) error { return nil }
func (r *plainRunner) Close() error                         { return nil }

func startRun(t *testing.T, a *agent.Agent) chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "start")) {
		}
	}()
	return done
}

// A steer reaches the run in flight.
func TestAgentSteersARunningTask(t *testing.T) {
	runner := &steerRunner{started: make(chan struct{}), release: make(chan struct{})}
	a, err := agent.NewAgent(agent.AgentOptions{
		NewRunner: func(context.Context, string, string) (agent.Runner, error) { return runner, nil },
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	done := startRun(t, a)
	<-runner.started
	steer := message.NewTextMessage(message.RoleUser, "also check the logs")
	if err := a.Steer(context.Background(), "c1", steer); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	runner.mu.Lock()
	taken := len(runner.steers)
	runner.mu.Unlock()
	if taken != 1 {
		t.Fatalf("steers taken = %d, want 1", taken)
	}
	close(runner.release)
	<-done
}

// A message for a conversation with no run belongs in Run, not Steer.
func TestAgentSteerRequiresARunInFlight(t *testing.T) {
	factory, _ := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	ctx := context.Background()
	if err := a.Steer(ctx, "never-seen", message.NewTextMessage(message.RoleUser, "hi")); !errors.Is(err, agent.ErrConversationIdle) {
		t.Errorf("unknown conversation = %v, want ErrConversationIdle", err)
	}
	drain(t, a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "one")))
	if err := a.Steer(ctx, "c1", message.NewTextMessage(message.RoleUser, "hi")); !errors.Is(err, agent.ErrConversationIdle) {
		t.Errorf("finished conversation = %v, want ErrConversationIdle", err)
	}
	if err := a.Steer(ctx, "", message.NewTextMessage(message.RoleUser, "hi")); !errors.Is(err, agent.ErrConversationRequired) {
		t.Errorf("empty conversation = %v, want ErrConversationRequired", err)
	}
}

// A paused task is idle: its next message is the answer, so steers are refused.
func TestAgentSteerRefusesAPausedTask(t *testing.T) {
	factory, _ := newFakeFactory(true)
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	ctx := context.Background()
	drain(t, a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "why?")))
	if err := a.Steer(ctx, "c1", message.NewTextMessage(message.RoleUser, "hi")); !errors.Is(err, agent.ErrConversationIdle) {
		t.Errorf("paused conversation = %v, want ErrConversationIdle", err)
	}
}

// An engine without Steer reports it instead of dropping the message.
func TestAgentSteerNeedsEngineSupport(t *testing.T) {
	runner := &plainRunner{started: make(chan struct{}), release: make(chan struct{})}
	a, err := agent.NewAgent(agent.AgentOptions{
		NewRunner: func(context.Context, string, string) (agent.Runner, error) { return runner, nil },
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	done := startRun(t, a)
	<-runner.started
	if err := a.Steer(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "hi")); !errors.Is(err, agent.ErrSteerUnsupported) {
		t.Fatalf("Steer = %v, want ErrSteerUnsupported", err)
	}
	close(runner.release)
	<-done
}

// A steer that arrives after its run ended is reported, not silently applied to
// whatever comes next.
func TestAgentSteerReportsARunThatEnded(t *testing.T) {
	runner := &steerRunner{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		steerErr: agent.ErrRunEnded,
	}
	a, err := agent.NewAgent(agent.AgentOptions{
		NewRunner: func(context.Context, string, string) (agent.Runner, error) { return runner, nil },
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	done := startRun(t, a)
	<-runner.started
	if err := a.Steer(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "hi")); !errors.Is(err, agent.ErrRunEnded) {
		t.Fatalf("Steer = %v, want ErrRunEnded", err)
	}
	close(runner.release)
	<-done
}
