package agent_test

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"

	agent "github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/message"
)

// hookRunner records what it was asked and reports the state the test chose.
type hookRunner struct {
	mu        sync.Mutex
	seen      []string
	state     agent.TaskState
	store     agent.Store
	taskID    string
	sessionID string
	results   agent.Result
}

func (r *hookRunner) State() agent.TaskState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *hookRunner) Run(_ context.Context, msg message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		r.mu.Lock()
		r.seen = append(r.seen, msg.Content.Text())
		if r.state == "" {
			r.state = agent.StateCompleted
		}
		state := r.state
		r.mu.Unlock()
		if r.store != nil && r.taskID != "" {
			_, _, _ = r.store.Save(context.Background(), agent.Checkpoint{
				Task: agent.Task{ID: r.taskID, SessionID: r.sessionID, State: state},
			}, nil)
		}
		yield(agent.Status{State: state}, nil)
	}
}

func (r *hookRunner) Result(context.Context) (agent.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := r.results
	result.State = r.state
	return result, nil
}

func (r *hookRunner) Cancel(context.Context, string) error { return nil }
func (r *hookRunner) Close() error                         { return nil }

func (r *hookRunner) inputs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func newHookAgent(t *testing.T, opts agent.AgentOptions, runner *hookRunner) *agent.Agent {
	t.Helper()
	if opts.Name == "" {
		opts.Name = "seat_5"
	}
	opts.NewRunner = func(_ context.Context, sessionID, taskID string) (agent.Runner, error) {
		runner.taskID = taskID
		runner.sessionID = sessionID
		runner.store = opts.Store
		return runner, nil
	}
	a, err := agent.NewAgent(opts)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// drainRun consumes one run and returns its events and the first error.
func drainRun(t *testing.T, a *agent.Agent, sessionID, text string) ([]agent.Event, error) {
	t.Helper()
	var events []agent.Event
	var firstErr error
	for event, err := range a.Run(context.Background(), sessionID, message.NewTextMessage(message.RoleUser, text)) {
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		events = append(events, event)
	}
	return events, firstErr
}

func TestBeforeTurnRewritesTheMessage(t *testing.T) {
	runner := &hookRunner{}
	a := newHookAgent(t, agent.AgentOptions{
		Hooks: agent.Hooks{
			BeforeTurn: func(_ context.Context, turn *agent.Turn) error {
				if turn.Agent != "seat_5" || turn.SessionID != "s1" {
					t.Fatalf("hook saw %+v", *turn)
				}
				turn.Message = message.NewTextMessage(message.RoleUser,
					"公开回顾：第1天无人出局\n"+turn.Message.Content.Text())
				return nil
			},
		},
	}, runner)
	if _, err := drainRun(t, a, "s1", "我投5号"); err != nil {
		t.Fatalf("run: %v", err)
	}
	seen := runner.inputs()
	if len(seen) != 1 || !strings.Contains(seen[0], "公开回顾") || !strings.HasSuffix(seen[0], "我投5号") {
		t.Fatalf("the engine saw %q, want the rewritten message", seen)
	}
}

func TestBeforeTurnFailureStopsTheTurn(t *testing.T) {
	runner := &hookRunner{}
	wantErr := errors.New("memory unavailable")
	a := newHookAgent(t, agent.AgentOptions{
		Hooks: agent.Hooks{BeforeTurn: func(context.Context, *agent.Turn) error { return wantErr }},
	}, runner)
	events, err := drainRun(t, a, "s1", "hi")
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want the hook's failure", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %v, the turn must not start", events)
	}
	if len(runner.inputs()) != 0 {
		t.Fatal("the engine ran a turn the hook refused")
	}
}

func TestAfterTurnSeesTheFinishedTurn(t *testing.T) {
	runner := &hookRunner{results: agent.Result{Content: message.NewTextContent("好的")}}
	var got agent.Turn
	var gotResult agent.Result
	a := newHookAgent(t, agent.AgentOptions{
		Hooks: agent.Hooks{AfterTurn: func(_ context.Context, turn agent.Turn, result agent.Result) error {
			got = turn
			gotResult = result
			return nil
		}},
	}, runner)
	if _, err := drainRun(t, a, "s1", "第一句"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got.Agent != "seat_5" || got.SessionID != "s1" || got.TaskID == "" {
		t.Fatalf("turn = %+v", got)
	}
	if got.Message.Content.Text() != "第一句" {
		t.Fatalf("message = %q", got.Message.Content.Text())
	}
	if gotResult.State != agent.StateCompleted || gotResult.TaskID != got.TaskID {
		t.Fatalf("result = %+v", gotResult)
	}
	if gotResult.Content.Text() != "好的" {
		t.Fatalf("content = %q, want the engine's answer", gotResult.Content.Text())
	}
}

func TestAfterTurnFailureLeavesTheRunClean(t *testing.T) {
	runner := &hookRunner{}
	a := newHookAgent(t, agent.AgentOptions{
		Hooks: agent.Hooks{AfterTurn: func(context.Context, agent.Turn, agent.Result) error {
			return errors.New("memory write failed")
		}},
	}, runner)
	events, err := drainRun(t, a, "s1", "hi")
	if err != nil {
		t.Fatalf("a failing after-turn hook must not fail the run: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %v, want the terminal status only", events)
	}
}

func TestOnSessionReportsNewAndResumed(t *testing.T) {
	store := agenttest.NewMemStore()
	runner := &hookRunner{state: agent.StateInputRequired}
	var seen []agent.SessionInfo
	var mu sync.Mutex
	hook := agent.Hooks{OnSession: func(_ context.Context, info agent.SessionInfo) error {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, info)
		return nil
	}}
	first := newHookAgent(t, agent.AgentOptions{Store: store, Hooks: hook}, runner)
	if _, err := drainRun(t, first, "s1", "夜间行动"); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// A second process opens the same session: the task is paused, so this is a
	// resume rather than a new task.
	second := newHookAgent(t, agent.AgentOptions{Store: store, Hooks: hook}, &hookRunner{})
	if _, err := drainRun(t, second, "s1", "answer"); err != nil {
		t.Fatalf("second run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("sessions = %d, want one per open", len(seen))
	}
	if seen[0].Resumed || seen[0].Agent != "seat_5" || seen[0].SessionID != "s1" {
		t.Fatalf("first session = %+v", seen[0])
	}
	if !seen[1].Resumed {
		t.Fatalf("second session = %+v, want Resumed", seen[1])
	}
	if seen[1].TaskID != seen[0].TaskID {
		t.Fatalf("task changed across the resume: %q then %q", seen[0].TaskID, seen[1].TaskID)
	}
}

func TestOnSessionFailureFailsTheOpen(t *testing.T) {
	runner := &hookRunner{}
	a := newHookAgent(t, agent.AgentOptions{
		Hooks: agent.Hooks{OnSession: func(context.Context, agent.SessionInfo) error {
			return errors.New("no memory for this seat")
		}},
	}, runner)
	_, err := drainRun(t, a, "s1", "hi")
	if err == nil || !strings.Contains(err.Error(), "no memory") {
		t.Fatalf("error = %v, want the hook's failure", err)
	}
	if len(runner.inputs()) != 0 {
		t.Fatal("the engine ran a session the host refused")
	}
}

func TestChainStopsAtTheFirstFailingStage(t *testing.T) {
	var order []string
	chained := agent.Chain(
		agent.Hooks{BeforeTurn: func(context.Context, *agent.Turn) error {
			order = append(order, "first")
			return errors.New("stop")
		}},
		agent.Hooks{BeforeTurn: func(context.Context, *agent.Turn) error {
			order = append(order, "second")
			return nil
		}},
		agent.Hooks{AfterTurn: func(context.Context, agent.Turn, agent.Result) error {
			order = append(order, "after")
			return errors.New("logged, not raised")
		}},
	)
	turn := &agent.Turn{}
	if err := chained.BeforeTurn(context.Background(), turn); err == nil {
		t.Fatal("a failing stage must stop the chain")
	}
	if len(order) != 1 || order[0] != "first" {
		t.Fatalf("order = %v, want only the first stage", order)
	}
	if err := chained.AfterTurn(context.Background(), agent.Turn{}, agent.Result{}); err != nil {
		t.Fatalf("a failing after-turn hook must not surface: %v", err)
	}
}
