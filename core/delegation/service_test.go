package delegation_test

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime/runctx"
	"github.com/GizClaw/flowcraft/core/tool"
)

type fakeSession struct {
	mu     sync.Mutex
	state  agent.TaskState
	result agent.Result
	runs   int
	run    func(*fakeSession, context.Context) iter.Seq2[agent.Event, error]
}

func (f *fakeSession) setState(state agent.TaskState) {
	f.mu.Lock()
	f.state = state
	f.mu.Unlock()
}

func (f *fakeSession) State(context.Context) (agent.TaskState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == "" {
		return agent.StateSubmitted, nil
	}
	return f.state, nil
}

func (f *fakeSession) Run(ctx context.Context, _ message.Message, _ ...agent.TurnOption) iter.Seq2[agent.Event, error] {
	f.mu.Lock()
	f.runs++
	run := f.run
	f.mu.Unlock()
	if run != nil {
		return run(f, ctx)
	}
	return func(yield func(agent.Event, error) bool) {
		f.setState(agent.StateWorking)
		if !yield(agent.Status{State: agent.StateWorking}, nil) {
			return
		}
		f.setState(agent.StateCompleted)
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}

func (f *fakeSession) Cancel(context.Context, string) error {
	f.setState(agent.StateCanceled)
	return nil
}

func (f *fakeSession) Result(context.Context) (agent.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.result, nil
}

type fakeSessions struct {
	names  []string
	byName map[string]*fakeSession
	lastID string
}

func (f *fakeSessions) Session(name, sessionID string) (delegation.Session, bool) {
	session, ok := f.byName[name]
	f.lastID = sessionID
	return session, ok
}

func (f *fakeSessions) Names() []string { return f.names }

func parentCtx(callID string) context.Context {
	ctx := runctx.WithRun(context.Background(), runctx.Run{
		SessionID: "s1",
		AgentName: "planner",
		TaskID:    "t1",
	})
	if callID == "" {
		return ctx
	}
	return tool.WithCallID(ctx, callID)
}

func newFake() (*fakeSessions, *fakeSession) {
	session := &fakeSession{
		state:  agent.StateSubmitted,
		result: agent.Result{State: agent.StateCompleted, Content: message.NewTextContent("done")},
	}
	return &fakeSessions{
		names:  []string{"planner", "worker"},
		byName: map[string]*fakeSession{"worker": session},
	}, session
}

func TestDelegateSyncReturnsTheChildAnswer(t *testing.T) {
	sessions, child := newFake()
	service, err := delegation.New(sessions, delegation.Policy{
		Targets: []string{"worker"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	outcome, err := service.Delegate(parentCtx("call-1"), delegation.Request{
		Target: "worker",
		Input:  "do it",
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if outcome.Status != delegation.StatusCompleted {
		t.Fatalf("status = %s", outcome.Status)
	}
	if outcome.Content.Text() != "done" {
		t.Fatalf("content = %q", outcome.Content.Text())
	}
	if outcome.ChildSession != "s1/d/worker/call-1" || sessions.lastID != outcome.ChildSession {
		t.Fatalf("child = %q, sessions saw %q", outcome.ChildSession, sessions.lastID)
	}
	if outcome.Depth != 1 || outcome.ChainID == "" {
		t.Fatalf("lineage = depth %d chain %q", outcome.Depth, outcome.ChainID)
	}
	if child.runs != 1 {
		t.Fatalf("runs = %d, want one", child.runs)
	}
}

func TestDelegateSyncReturnsTheQuestion(t *testing.T) {
	sessions, child := newFake()
	question := message.NewTextContent("which one?")
	child.result = agent.Result{State: agent.StateInputRequired, Question: &question}
	child.run = func(f *fakeSession, _ context.Context) iter.Seq2[agent.Event, error] {
		return func(yield func(agent.Event, error) bool) {
			f.setState(agent.StateInputRequired)
			yield(agent.Status{State: agent.StateInputRequired, Reason: "which one?"}, nil)
		}
	}
	service, _ := delegation.New(sessions, delegation.Policy{Targets: []string{"worker"}})
	outcome, err := service.Delegate(parentCtx("call-1"), delegation.Request{
		Target: "worker", Input: "do it",
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if outcome.Status != delegation.StatusInputRequired {
		t.Fatalf("status = %s", outcome.Status)
	}
	if outcome.Question == nil || outcome.Question.Text() != "which one?" {
		t.Fatalf("question = %+v", outcome.Question)
	}
}

func TestDelegateRetryReadsTheTerminalChild(t *testing.T) {
	sessions, child := newFake()
	service, _ := delegation.New(sessions, delegation.Policy{Targets: []string{"worker"}})
	ctx := parentCtx("call-1")
	if _, err := service.Delegate(ctx, delegation.Request{Target: "worker", Input: "do it"}); err != nil {
		t.Fatalf("first Delegate: %v", err)
	}
	outcome, err := service.Delegate(ctx, delegation.Request{Target: "worker", Input: "do it"})
	if err != nil {
		t.Fatalf("second Delegate: %v", err)
	}
	if outcome.Status != delegation.StatusCompleted || child.runs != 1 {
		t.Fatalf("status/runs = %s/%d", outcome.Status, child.runs)
	}
	if outcome.Depth != 1 || outcome.ChainID == "" {
		t.Fatalf("retry lineage = depth %d chain %q", outcome.Depth, outcome.ChainID)
	}
}

func TestDelegateRejectsDisallowedTargets(t *testing.T) {
	sessions, _ := newFake()
	service, _ := delegation.New(sessions, delegation.Policy{Targets: []string{"other"}})
	for name, target := range map[string]string{
		"not in allowlist": "worker",
		"self":             "planner",
		"unknown":          "ghost",
	} {
		if _, err := service.Delegate(parentCtx("call-1"), delegation.Request{
			Target: target, Input: "do it",
		}); err == nil {
			t.Errorf("%s: Delegate accepted %q", name, target)
		}
	}
}

func TestDelegateRejectsDepthAndInputOverflow(t *testing.T) {
	sessions, _ := newFake()
	service, _ := delegation.New(sessions, delegation.Policy{
		Targets:       []string{"worker"},
		MaxDepth:      4,
		MaxInputBytes: 3,
	})
	depthCtx := delegation.WithCall(parentCtx("call-1"), delegation.Call{Depth: 4})
	if _, err := service.Delegate(depthCtx, delegation.Request{
		Target: "worker", Input: "ok",
	}); err == nil {
		t.Fatal("Delegate accepted the depth limit")
	}
	if _, err := service.Delegate(parentCtx("call-1"), delegation.Request{
		Target: "worker", Input: "four",
	}); err == nil {
		t.Fatal("Delegate accepted oversized input")
	}
}

func TestDelegateAsyncIsNotAvailableYet(t *testing.T) {
	sessions, _ := newFake()
	service, _ := delegation.New(sessions, delegation.Policy{
		Targets: []string{"worker"},
		Modes:   []delegation.Mode{delegation.ModeSync, delegation.ModeAsync},
	})
	_, err := service.Delegate(parentCtx("call-1"), delegation.Request{
		Mode: delegation.ModeAsync, Target: "worker", Input: "do it",
	})
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("error = %v, want not implemented", err)
	}
}

func TestDelegateRequiresACallID(t *testing.T) {
	sessions, _ := newFake()
	service, _ := delegation.New(sessions, delegation.Policy{Targets: []string{"worker"}})
	if _, err := service.Delegate(parentCtx(""), delegation.Request{
		Target: "worker", Input: "do it",
	}); err == nil {
		t.Fatal("Delegate accepted a missing call id")
	}
}

func TestDelegateInheritsTheChain(t *testing.T) {
	sessions, _ := newFake()
	service, _ := delegation.New(sessions, delegation.Policy{Targets: []string{"worker"}})
	ctx := delegation.WithCall(parentCtx("call-2"), delegation.Call{
		Depth:   2,
		ChainID: "chain-1",
	})
	outcome, err := service.Delegate(ctx, delegation.Request{Target: "worker", Input: "do it"})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if outcome.Depth != 3 || outcome.ChainID != "chain-1" {
		t.Fatalf("lineage = depth %d chain %q", outcome.Depth, outcome.ChainID)
	}
}
