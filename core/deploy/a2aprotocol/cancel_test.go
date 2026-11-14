package a2aprotocol

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

func cancelExecutor(t *testing.T) (*executor, *cancelRunner) {
	t.Helper()
	runner := &cancelRunner{started: make(chan struct{})}
	executor, err := newExecutor(executorConfig{
		Card:  cardConfig{Name: "x", Version: "1.0.0", URL: "https://x"},
		Agent: tableFor(t, runner),
	})
	if err != nil {
		t.Fatalf("newExecutor: %v", err)
	}
	return executor, runner
}

// A cancel reaches the running execution, and the run itself reports the
// canceled state: a second terminal event would be rejected by the server.
func TestCancelSignalsTheRunningExecution(t *testing.T) {
	executor, runner := cancelExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	execCtx := execContext()
	done := make(chan struct{})
	var last a2a.TaskState
	go func() {
		defer close(done)
		for event, err := range executor.Execute(ctx, execCtx) {
			if err != nil {
				t.Errorf("execute: %v", err)
				return
			}
			if update, ok := event.(*a2a.TaskStatusUpdateEvent); ok {
				last = update.Status.State
			}
		}
	}()
	<-runner.started

	signalled := 0
	for range executor.Cancel(ctx, execCtx) {
		signalled++
	}
	if signalled != 0 {
		t.Errorf("Cancel emitted %d events while the run was live", signalled)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the run did not stop after Cancel")
	}
	if last != a2a.TaskStateCanceled {
		t.Errorf("last state = %q, want canceled from the run", last)
	}
	if !runner.stopped.Load() {
		t.Error("the run's context was never cancelled")
	}
}

// Cancelling a task with nothing in flight answers directly, because no run will
// speak for it.
func TestCancelOfAnIdleTaskAnswers(t *testing.T) {
	executor, _ := cancelExecutor(t)
	var state a2a.TaskState
	for event, err := range executor.Cancel(context.Background(), execContext()) {
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		state = event.(*a2a.TaskStatusUpdateEvent).Status.State
	}
	if state != a2a.TaskStateCanceled {
		t.Errorf("state = %q, want canceled", state)
	}
}

// A cancel the engine refuses is reported, not papered over with a canceled
// status the checkpoint would contradict.
func TestCancelReportsAFailedCancel(t *testing.T) {
	store := agenttest.NewMemStore()
	if _, _, err := store.Save(context.Background(), agent.Checkpoint{
		Schema: agent.CheckpointSchema,
		Task:   agent.Task{ID: "task-1", SessionID: "conv-1", State: agent.StateInputRequired},
	}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	table, err := agent.NewAgent(agent.AgentOptions{
		Store: store,
		NewRunner: func(context.Context, string, string) (agent.Runner, error) {
			return failCancelRunner{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = table.Close() })
	executor, err := newExecutor(executorConfig{
		Card:  cardConfig{Name: "x", Version: "1", URL: "https://x"},
		Agent: table,
	})
	if err != nil {
		t.Fatalf("newExecutor: %v", err)
	}
	var cancelled int
	var seen error
	for event, err := range executor.Cancel(context.Background(), execContext()) {
		if err != nil {
			seen = err
			continue
		}
		if _, ok := event.(*a2a.TaskStatusUpdateEvent); ok {
			cancelled++
		}
	}
	if seen == nil || !strings.Contains(seen.Error(), "refused to cancel") {
		t.Fatalf("error = %v, want the engine's failure", seen)
	}
	if cancelled != 0 {
		t.Errorf("emitted %d canceled updates after the engine refused", cancelled)
	}
}

// A task with nothing to cancel still answers with the state the client asked
// for.
func TestCancelReportsTheRequestedState(t *testing.T) {
	executor, err := newExecutor(executorConfig{
		Card:  cardConfig{Name: "x", Version: "1", URL: "https://x"},
		Agent: tableFor(t, &completedRunner{}),
	})
	if err != nil {
		t.Fatalf("newExecutor: %v", err)
	}
	exec := &a2asrv.ExecutorContext{TaskID: "task-1", ContextID: "conv-1"}
	var state a2a.TaskState
	for event, err := range executor.Cancel(context.Background(), exec) {
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		state = event.(*a2a.TaskStatusUpdateEvent).Status.State
	}
	if state != a2a.TaskStateCanceled {
		t.Errorf("state = %q, want canceled", state)
	}
}
