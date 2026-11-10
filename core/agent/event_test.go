package agent

import (
	"context"
	"iter"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// nopRunner finishes one round without touching the network.
type nopRunner struct{}

func (nopRunner) State() TaskState { return StateSubmitted }
func (nopRunner) Run(context.Context, message.Message) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		yield(Status{State: StateCompleted}, nil)
	}
}
func (nopRunner) Cancel(context.Context, string) error { return nil }
func (nopRunner) Close() error                         { return nil }

func TestEventKinds(t *testing.T) {
	cases := []struct {
		event Event
		kind  EventKind
	}{
		{StreamStart{Round: 1}, KindStreamStart},
		{Text{Text: "x"}, KindText},
		{Reasoning{Text: "x"}, KindReasoning},
		{ToolCallStart{}, KindToolCall},
		{ToolResult{}, KindToolResult},
		{ArtifactWritten{}, KindArtifact},
		{UsageEvent{}, KindUsage},
		{Status{State: StateWorking}, KindStatus},
	}
	seen := make(map[EventKind]struct{}, len(cases))
	for _, c := range cases {
		if got := c.event.Kind(); got != c.kind {
			t.Errorf("%T.Kind() = %q, want %q", c.event, got, c.kind)
		}
		// The seal: every concrete kind implements event(), and only these do.
		c.event.event()
		if _, duplicate := seen[c.kind]; duplicate {
			t.Errorf("kind %q is claimed twice", c.kind)
		}
		seen[c.kind] = struct{}{}
	}
}

// An unknown packed code is a state this build does not know; reporting the
// start of the task is the honest fallback.
func TestStateFromCodeFallsBackToSubmitted(t *testing.T) {
	if got := stateFromCode(99); got != StateSubmitted {
		t.Errorf("stateFromCode(99) = %q, want submitted", got)
	}
	if got := stateFromCode(stateCode(StateCanceled)); got != StateCanceled {
		t.Errorf("round trip = %q, want canceled", got)
	}
}

// Close drops the table, so a closed agent keeps no session's runner reachable.
func TestCloseDropsTheTable(t *testing.T) {
	a, err := NewAgent(AgentOptions{
		NewRunner: func(context.Context, string, string) (Runner, error) {
			return nopRunner{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	for range a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "hi")) {
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	shard := a.table.shard("c1")
	shard.mu.Lock()
	entries := len(shard.entries)
	shard.mu.Unlock()
	if entries != 0 {
		t.Errorf("table still holds %d entries after Close", entries)
	}
}
