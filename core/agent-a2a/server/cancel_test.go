package server

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// cancelRunner blocks until its context is cancelled, then reports the state a
// real loop reports.
type cancelRunner struct {
	started   chan struct{}
	once      sync.Once
	cancelled atomic.Bool
}

func (r *cancelRunner) State() agent.TaskState { return agent.StateWorking }

func (r *cancelRunner) Run(ctx context.Context, _ message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		r.once.Do(func() { close(r.started) })
		if !yield(agent.Status{State: agent.StateWorking}, nil) {
			return
		}
		<-ctx.Done()
		yield(agent.Status{State: agent.StateCanceled}, nil)
	}
}

func (r *cancelRunner) Cancel(context.Context, string) error {
	r.cancelled.Store(true)
	return nil
}

func (r *cancelRunner) Close() error { return nil }

func cancelExecutor(t *testing.T) (*Executor, *cancelRunner) {
	t.Helper()
	runner := &cancelRunner{started: make(chan struct{})}
	executor, err := New(Config{
		Card: CardConfig{Name: "x", Version: "1.0.0", URL: "https://x"},
		Sessions: func(context.Context, *a2asrv.ExecutorContext) (agent.Runner, error) {
			return runner, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return executor, runner
}

func execContext() *a2asrv.ExecutorContext {
	return &a2asrv.ExecutorContext{
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi")),
		TaskID:    "task-1",
		ContextID: "conv-1",
	}
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
	if !runner.cancelled.Load() {
		t.Error("Cancel never reached the runner")
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

// failCancelRunner refuses to stop.
type failCancelRunner struct{}

func (failCancelRunner) State() agent.TaskState { return agent.StateWorking }
func (failCancelRunner) Run(context.Context, message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}
func (failCancelRunner) Cancel(context.Context, string) error {
	return errors.New("the engine refused to cancel")
}
func (failCancelRunner) Close() error { return nil }

// A cancel the engine refuses is reported, not papered over with a canceled
// status the checkpoint would contradict.
func TestCancelReportsAFailedCancel(t *testing.T) {
	executor, err := New(Config{
		Card: CardConfig{Name: "x", Version: "1", URL: "https://x"},
		Sessions: func(context.Context, *a2asrv.ExecutorContext) (agent.Runner, error) {
			return failCancelRunner{}, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
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
