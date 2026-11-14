package graph

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/message"
)

// A compiled graph is an engine: it satisfies the same contract a remote agent
// does, so a deployment can pick either without special cases.
func TestRunnerSatisfiesTheAgentContract(t *testing.T) {
	agenttest.Runner(t, func(t *testing.T) agent.Runner {
		g := build(t, `
version: 1
entry: a
nodes:
  - {id: a, kind: count}
edges:
  - {from: a, to: end}
`, &countKind{})
		return NewRunner(g, RunnerConfig{SessionID: "conv-1", TaskID: "task-1"})
	})
}

// cancelDoc suspends once and finishes on the answer.
const cancelDoc = `
version: 1
entry: ask
state:
  text: {type: text}
nodes:
  - {id: ask, kind: ask, config: {key: text}}
edges:
  - {from: ask, to: end}
output: {key: text}
`

// A fresh process has no run in memory. Cancel must reach the stored task
// through the checkpoint, or a restart resumes a task nobody is waiting for.
func TestCancelAfterRestartCancelsTheStoredTask(t *testing.T) {
	store := agenttest.NewMemStore()
	g := build(t, cancelDoc, askKind{})
	config := RunnerConfig{
		Store: store, Identity: "sha256:deployment",
		SessionID: "conv-1", TaskID: "task-1",
	}
	first := NewRunner(g, config)
	for _, err := range first.Run(context.Background(), message.NewTextMessage(message.RoleUser, "hi")) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	if state := first.State(); state != agent.StateInputRequired {
		t.Fatalf("state = %s, want input-required", state)
	}
	// A new runner over the same checkpoint, as a restarted process would have.
	second := NewRunner(g, config)
	if err := second.Cancel(context.Background(), "user canceled"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if state := second.State(); state != agent.StateCanceled {
		t.Errorf("runner state = %s, want canceled", state)
	}
	checkpoint, err := store.Load(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if checkpoint.Task.State != agent.StateCanceled {
		t.Errorf("checkpoint state = %s, want canceled", checkpoint.Task.State)
	}
}

// A cancel with nothing stored is a no-op, not an error: the task may never
// have started.
func TestCancelWithNothingStoredIsANoOp(t *testing.T) {
	g := build(t, cancelDoc, askKind{})
	runner := NewRunner(g, RunnerConfig{
		Store: agenttest.NewMemStore(), SessionID: "conv-1", TaskID: "absent",
	})
	if err := runner.Cancel(context.Background(), "nothing to stop"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
}

// The timeout bounds the whole run, suspension included: a task that waited past
// its budget is not resumed as if it had just started.
func TestTimeoutCoversTheSuspendedTime(t *testing.T) {
	store := agenttest.NewMemStore()
	g := build(t, `
version: 1
limits: {timeout: 40ms}
entry: ask
state:
  text: {type: text}
nodes:
  - {id: ask, kind: ask, config: {key: text}}
edges:
  - {from: ask, to: end}
output: {key: text}
`, askKind{})
	config := RunnerConfig{
		Store: store, Identity: "sha256:deployment",
		SessionID: "conv-1", TaskID: "task-1",
	}
	first := NewRunner(g, config)
	for _, err := range first.Run(context.Background(), message.NewTextMessage(message.RoleUser, "hi")) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	if state := first.State(); state != agent.StateInputRequired {
		t.Fatalf("state = %s, want input-required", state)
	}
	time.Sleep(60 * time.Millisecond)
	second := NewRunner(g, config)
	var canceled bool
	var runErr error
	for event, err := range second.Run(context.Background(), message.NewTextMessage(message.RoleUser, "draft")) {
		if err != nil {
			runErr = err
			continue
		}
		if status, ok := event.(agent.Status); ok && status.State == agent.StateCanceled {
			canceled = true
		}
	}
	if !canceled {
		t.Error("a restored run past its budget was resumed")
	}
	if !errors.Is(runErr, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", runErr)
	}
}
