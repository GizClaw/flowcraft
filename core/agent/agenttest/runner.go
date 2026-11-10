// Package agenttest is the conformance suite for the agent's contracts: the
// Store that makes a conversation durable, and the Runner an engine implements.
package agenttest

import (
	"context"
	"iter"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// Runner runs the runner suite against one implementation. build must return a
// fresh, idle runner for each subtest.
//
// The suite pins only what a caller may rely on: a fresh runner is not terminal,
// one Run ends in a terminal state, a task that paused continues on the next Run,
// a cancelled context terminates the sequence, cancel never leaves a task
// running, and Close is idempotent. It says nothing about how an engine reaches
// those states — a graph, a proxy over a peer and a fake all satisfy it the same
// way.
func Runner(t *testing.T, build func(t *testing.T) agent.Runner) {
	t.Helper()
	cases := []struct {
		name string
		run  func(*testing.T, func(*testing.T) agent.Runner)
	}{
		{"FreshRunnerIsNotTerminal", freshRunnerIsNotTerminal},
		{"RunEndsInATerminalState", runEndsInATerminalState},
		{"PausedTaskContinuesOnTheNextRun", pausedTaskContinues},
		{"CancelledContextTerminates", cancelledContextTerminates},
		{"CancelNeverLeavesATaskRunning", cancelNeverLeavesATaskRunning},
		{"CancelAfterCompletionIsANoOp", cancelAfterCompletionIsANoOp},
		{"CancelIsIdempotent", cancelIsIdempotent},
		{"CloseIsIdempotent", closeIsIdempotent},
		{"RunAfterTerminalStateDoesNotHang", runAfterTerminalDoesNotHang},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, build) })
	}
}

// turn is one user message.
func turn() message.Message {
	return message.NewTextMessage(message.RoleUser, "hi")
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

func freshRunnerIsNotTerminal(t *testing.T, build func(*testing.T) agent.Runner) {
	if state := build(t).State(); state.Terminal() {
		t.Fatalf("fresh runner state = %s, want a non-terminal state", state)
	}
}

func runEndsInATerminalState(t *testing.T, build func(*testing.T) agent.Runner) {
	runner := build(t)
	events, err := drain(runner.Run(context.Background(), turn()))
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

// A task that ends its first Run waiting for input continues with the same
// entry: there is no Resume in the contract, so the next Run must carry it to a
// terminal state. A runner that never pauses has nothing to prove here.
func pausedTaskContinues(t *testing.T, build func(*testing.T) agent.Runner) {
	runner := build(t)
	events, err := drain(runner.Run(context.Background(), turn()))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	status, ok := lastStatus(events)
	if !ok || status.State != agent.StateInputRequired {
		t.Skipf("the runner did not pause (last status %+v)", status)
	}
	events, err = drain(runner.Run(context.Background(), turn()))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	status, ok = lastStatus(events)
	if !ok || !status.State.Terminal() {
		t.Fatalf("a paused task did not continue to a terminal state: %+v", events)
	}
	if got := runner.State(); got != status.State {
		t.Errorf("State() = %s after reporting %s", got, status.State)
	}
}

// A cancelled context must end the sequence: an error or a terminal status. It
// must not report success as a way of finishing early, but it may legitimately
// complete first — the contract gives no ordering between a cancel and a fast
// implementation's last status.
func cancelledContextTerminates(t *testing.T, build func(*testing.T) agent.Runner) {
	runner := build(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	events, err := drain(runner.Run(ctx, turn()))
	if err != nil {
		return
	}
	status, ok := lastStatus(events)
	if !ok || !status.State.Terminal() {
		t.Fatalf("cancelled Run ended without a terminal status: %+v", events)
	}
}

// Cancel never leaves a task working: either it stops the task, or the task had
// nothing to stop and the state is unchanged.
func cancelNeverLeavesATaskRunning(t *testing.T, build func(*testing.T) agent.Runner) {
	runner := build(t)
	if err := runner.Cancel(context.Background(), "test"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if state := runner.State(); state == agent.StateWorking {
		t.Fatalf("Cancel left the task %s", state)
	}
}

func cancelAfterCompletionIsANoOp(t *testing.T, build func(*testing.T) agent.Runner) {
	runner := build(t)
	if _, err := drain(runner.Run(context.Background(), turn())); err != nil {
		t.Fatalf("Run: %v", err)
	}
	before := runner.State()
	if err := runner.Cancel(context.Background(), "too late"); err != nil {
		t.Fatalf("Cancel after completion: %v", err)
	}
	if after := runner.State(); after != before {
		t.Errorf("Cancel after completion moved the state from %s to %s", before, after)
	}
}

func cancelIsIdempotent(t *testing.T, build func(*testing.T) agent.Runner) {
	runner := build(t)
	if err := runner.Cancel(context.Background(), "first"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := runner.Cancel(context.Background(), "second"); err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
}

func closeIsIdempotent(t *testing.T, build func(*testing.T) agent.Runner) {
	runner := build(t)
	if err := runner.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := runner.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// A second Run on a finished task is either a no-op that reports the terminal
// state or an error; what it must never do is hang or report a working task.
func runAfterTerminalDoesNotHang(t *testing.T, build func(*testing.T) agent.Runner) {
	runner := build(t)
	if _, err := drain(runner.Run(context.Background(), turn())); err != nil {
		t.Fatalf("Run: %v", err)
	}
	events, err := drain(runner.Run(context.Background(), turn()))
	if err != nil {
		return
	}
	status, ok := lastStatus(events)
	if !ok || !status.State.Terminal() {
		t.Fatalf("second Run ended without a terminal status: %+v", events)
	}
}
