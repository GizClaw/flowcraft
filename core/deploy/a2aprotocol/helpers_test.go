package a2aprotocol

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// settingsJSON turns a Go literal into the JSON a settings block now is.
func settingsJSON(value any) json.RawMessage {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return body
}

func testInfo() a2a.TaskInfo {
	return a2a.TaskInfo{TaskID: "task-1", ContextID: "conv-1"}
}

func execContext() *a2asrv.ExecutorContext {
	return &a2asrv.ExecutorContext{
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi")),
		TaskID:    "task-1",
		ContextID: "conv-1",
	}
}

// tableFor builds a one-session table whose engine returns runner.
func tableFor(t *testing.T, runner agent.Runner) *agent.Agent {
	t.Helper()
	table, err := agent.NewAgent(agent.AgentOptions{
		NewRunner: func(context.Context, string, string) (agent.Runner, error) {
			return runner, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = table.Close() })
	return table
}

// completedRunner reports a whole task in one round, and remembers the state it
// last announced so the table can report it.
type completedRunner struct {
	mu    sync.Mutex
	state agent.TaskState
}

func (r *completedRunner) State() agent.TaskState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == "" {
		return agent.StateSubmitted
	}
	return r.state
}

func (r *completedRunner) Run(_ context.Context, _ message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		r.mu.Lock()
		r.state = agent.StateWorking
		r.mu.Unlock()
		if !yield(agent.Status{State: agent.StateWorking}, nil) {
			return
		}
		r.mu.Lock()
		r.state = agent.StateCompleted
		r.mu.Unlock()
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}
func (*completedRunner) Cancel(context.Context, string) error { return nil }
func (*completedRunner) Close() error                         { return nil }

// cancelRunner blocks until its context is cancelled, then reports the state a
// real loop reports.
type cancelRunner struct {
	started chan struct{}
	once    sync.Once
	stopped atomic.Bool
}

func (r *cancelRunner) State() agent.TaskState { return agent.StateWorking }

func (r *cancelRunner) Run(ctx context.Context, _ message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		r.once.Do(func() { close(r.started) })
		if !yield(agent.Status{State: agent.StateWorking}, nil) {
			return
		}
		<-ctx.Done()
		r.stopped.Store(true)
		yield(agent.Status{State: agent.StateCanceled}, nil)
	}
}

func (r *cancelRunner) Cancel(context.Context, string) error { return nil }
func (r *cancelRunner) Close() error                         { return nil }

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

// failedRunner reports the failure its step produced.
type failedRunner struct{ err error }

func (r *failedRunner) State() agent.TaskState { return agent.StateWorking }

func (r *failedRunner) Run(context.Context, message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		yield(nil, r.err)
	}
}

func (*failedRunner) Cancel(context.Context, string) error { return nil }
func (*failedRunner) Close() error                         { return nil }
