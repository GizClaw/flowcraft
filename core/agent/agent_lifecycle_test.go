package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestNewAgentRequiresAFactory(t *testing.T) {
	if _, err := agent.NewAgent(agent.AgentOptions{}); err == nil {
		t.Fatal("an agent without a runner factory was accepted")
	}
}

// A closed agent answers no reads and cancels nothing: the table is gone, and
// opening a stored session after shutdown would resurrect it.
func TestStateAndCancelAfterClose(t *testing.T) {
	factory, _ := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	for range a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "hi")) {
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := a.State(context.Background(), "c1"); !errors.Is(err, agent.ErrClosed) {
		t.Errorf("State after Close = %v, want ErrClosed", err)
	}
	if err := a.Cancel(context.Background(), "c1", "done"); !errors.Is(err, agent.ErrClosed) {
		t.Errorf("Cancel after Close = %v, want ErrClosed", err)
	}
}

// Drain reports what is still running, by session, so a caller can decide
// whether a restart is safe.
func TestDrainReportsBusySessions(t *testing.T) {
	started := make(chan struct{}, 1)
	runner := &fakeRunner{state: agent.StateSubmitted, block: true}
	a, err := agent.NewAgent(agent.AgentOptions{
		NewRunner: func(context.Context, string, string) (agent.Runner, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			return runner, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "hi")) {
		}
	}()
	<-started
	// Wait for the run to actually take the slot.
	for runner.calls() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = a.Drain(ctx)
	if err == nil {
		t.Fatal("Drain accepted a run in flight")
	}
	if !strings.Contains(err.Error(), "c1") {
		t.Errorf("Drain error = %v, want the busy session named", err)
	}
	_ = a.Close()
	<-done
}

// A cancel that lands while the runner is still being built waits for the
// opening to finish, and gives up when its own context does.
func TestCancelWhileOpeningHonoursTheContext(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	a, err := agent.NewAgent(agent.AgentOptions{
		NewRunner: func(context.Context, string, string) (agent.Runner, error) {
			close(entered)
			<-release
			return &fakeRunner{state: agent.StateSubmitted}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	go func() {
		for range a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "hi")) {
		}
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Cancel(ctx, "c1", "stop"); !errors.Is(err, context.Canceled) {
		t.Errorf("Cancel while opening = %v, want the context's error", err)
	}
	close(release)
	_ = a.Close()
}
