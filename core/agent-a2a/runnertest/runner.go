// Package runnertest is the conformance suite for a2a.Runner.
//
// The interface has two real implementations — *agent.Session and the deploy
// layer's proxy over a remote agent — and the protocol adapter must not be able
// to tell them apart. The suite pins only what the executor relies on: a runner
// reports a state, starts a Run that ends in a terminal state, refuses a Resume
// that has nothing to resume, and stops when its context is cancelled.
package runnertest

import (
	"context"
	"iter"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	a2a "github.com/GizClaw/flowcraft/core/agent-a2a"
	"github.com/GizClaw/flowcraft/core/message"
)

// Runner runs the suite against one implementation. build must return a fresh,
// idle runner for each subtest.
func Runner(t *testing.T, build func(t *testing.T) a2a.Runner) {
	t.Helper()
	cases := []struct {
		name string
		run  func(*testing.T, func(*testing.T) a2a.Runner)
	}{
		{"FreshRunnerIsNotTerminal", freshRunnerIsNotTerminal},
		{"RunEndsInATerminalState", runEndsInATerminalState},
		{"ResumeWithoutAPauseFails", resumeWithoutAPauseFails},
		{"CancelledRunTerminates", cancelledRunTerminates},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, build) })
	}
}

// input is one user message.
func input() agent.Input {
	return agent.Input{Messages: []message.Message{
		message.NewTextMessage(message.RoleUser, "hi"),
	}}
}

// drain consumes a sequence and returns its events plus the first error.
func drain(seq iter.Seq2[agent.Event, error]) ([]agent.Event, error) {
	var events []agent.Event
	for event, err := range seq {
		if err != nil {
			return events, err
		}
		events = append(events, event)
	}
	return events, nil
}

// lastStatus returns the newest Status event.
func lastStatus(events []agent.Event) (agent.Status, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if status, ok := events[i].(agent.Status); ok {
			return status, true
		}
	}
	return agent.Status{}, false
}

func freshRunnerIsNotTerminal(t *testing.T, build func(*testing.T) a2a.Runner) {
	runner := build(t)
	if state := runner.State(); state.Terminal() {
		t.Fatalf("fresh runner state = %s, want a non-terminal state", state)
	}
}

func runEndsInATerminalState(t *testing.T, build func(*testing.T) a2a.Runner) {
	runner := build(t)
	runner.SetIdentity("task-1", "conv-1")
	events, err := drain(runner.Run(context.Background(), input()))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	status, ok := lastStatus(events)
	if !ok {
		t.Fatalf("Run produced no Status event: %+v", events)
	}
	if !status.State.Terminal() {
		t.Fatalf("final status = %s, want a terminal state", status.State)
	}
	if got := runner.State(); got != status.State {
		t.Errorf("State() = %s after reporting %s", got, status.State)
	}
}

func resumeWithoutAPauseFails(t *testing.T, build func(*testing.T) a2a.Runner) {
	runner := build(t)
	_, err := drain(runner.Resume(context.Background(),
		agent.Reply{Content: message.NewTextContent("an answer")}))
	if err == nil {
		t.Fatal("Resume without a pause = nil, want an error")
	}
}

// A cancelled context must end the sequence: an error or a terminal status. It
// must not report success as a way of finishing early, but it may legitimately
// complete first — the interface gives no ordering between a cancel and a fast
// implementation's last status.
func cancelledRunTerminates(t *testing.T, build func(*testing.T) a2a.Runner) {
	runner := build(t)
	runner.SetIdentity("task-2", "conv-2")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	events, err := drain(runner.Run(ctx, input()))
	if err != nil {
		return
	}
	status, ok := lastStatus(events)
	if !ok || !status.State.Terminal() {
		t.Fatalf("cancelled Run ended without a terminal status: %+v", events)
	}
}
