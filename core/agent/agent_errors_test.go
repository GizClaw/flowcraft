package agent_test

import (
	"context"
	"errors"
	"iter"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// errorRunner fails before producing anything.
type errorRunner struct{ err error }

func (r *errorRunner) State() agent.TaskState { return agent.StateFailed }
func (r *errorRunner) Run(context.Context, message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) { yield(nil, r.err) }
}
func (r *errorRunner) Cancel(context.Context, string) error { return nil }
func (r *errorRunner) Close() error                         { return nil }

// A runner's failure reaches the caller unchanged.
func TestAgentRunPropagatesRunnerErrors(t *testing.T) {
	want := errors.New("model exploded")
	a, err := agent.NewAgent(agent.AgentOptions{
		NewRunner: func(context.Context, string, string) (agent.Runner, error) {
			return &errorRunner{err: want}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if got := firstError(a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "hi"))); !errors.Is(got, want) {
		t.Fatalf("error = %v, want %v", got, want)
	}
}

// A consumer that stops reading cancels the run: the step already ran, so
// letting it continue would spend on a task nobody reads.
func TestAgentCancelsWhenTheConsumerStops(t *testing.T) {
	factory, built := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	seen := 0
	for range a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "hi")) {
		seen++
		break
	}
	if seen != 1 {
		t.Fatalf("read %d events, want one", seen)
	}
	if runner := (*built)[0]; runner.cancelCalls != 1 {
		t.Errorf("cancel calls = %d, want the abandoned run cancelled", runner.cancelCalls)
	}
}

// Drain stops new work without cancelling what is in flight; Close is
// idempotent and stops accepting work forever.
func TestAgentDrainAndClose(t *testing.T) {
	factory, _ := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	ctx := context.Background()
	if err := a.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if err := firstError(a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "hi"))); !errors.Is(err, agent.ErrClosed) {
		t.Fatalf("run after Drain = %v, want ErrClosed", err)
	}
	if err := a.Drain(ctx); err != nil {
		t.Errorf("second Drain = %v, want nil", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// Cancelling nothing is not an error, and an empty conversation id is refused
// before anything is touched.
func TestAgentCancelWithNothingToCancel(t *testing.T) {
	factory, built := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	ctx := context.Background()
	if err := a.Cancel(ctx, "never-seen", "nobody"); err != nil {
		t.Errorf("Cancel unknown = %v, want nil", err)
	}
	if len(*built) != 0 {
		t.Errorf("Cancel built a runner for a conversation that does not exist")
	}
	if err := a.Cancel(ctx, "", "nobody"); !errors.Is(err, agent.ErrConversationRequired) {
		t.Errorf("Cancel empty = %v, want ErrConversationRequired", err)
	}
}

// A protocol addressing another task than the one the conversation is on is
// refused rather than silently starting something else.
func TestAgentRejectsAMismatchedTaskID(t *testing.T) {
	factory, _ := newFakeFactory(true)
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	ctx := context.Background()
	drain(t, a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "why?")))
	err = firstError(a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "hi"), agent.WithTaskID("someone-else")))
	if !errors.Is(err, agent.ErrTaskMismatch) {
		t.Fatalf("error = %v, want ErrTaskMismatch", err)
	}
}

// State needs a conversation to name, like every other entry point.
func TestAgentStateRequiresAConversation(t *testing.T) {
	factory, _ := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if _, err := a.State(context.Background(), ""); !errors.Is(err, agent.ErrConversationRequired) {
		t.Fatalf("State empty = %v, want ErrConversationRequired", err)
	}
}
