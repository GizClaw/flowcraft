package agenttest

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// PreparerFactory builds a fresh [agent.Preparer] for each subtest so
// subtests do not share implementation state.
type PreparerFactory func() agent.Preparer

// PreparerSuite runs every contract probe that applies to the
// [agent.Preparer] produced by f:
//
//   - zero inputs must not panic;
//   - a successful Before MUST return a non-nil, fresh Board (the
//     engine mutates the returned board in place, so returning the
//     input aliases the previous stage's state);
//   - the previous board must not be mutated on the success path;
//   - Before MUST honour ctx cancellation and return promptly.
func PreparerSuite(t *testing.T, f PreparerFactory) {
	t.Helper()

	t.Run("ZeroInputNoPanic", func(t *testing.T) { preparerZeroInput(t, f) })
	t.Run("NonNilBoardOnSuccess", func(t *testing.T) { preparerNonNilBoard(t, f) })
	t.Run("FreshBoardPerCall", func(t *testing.T) { preparerFreshBoard(t, f) })
	t.Run("DoesNotMutatePrevious", func(t *testing.T) { preparerNoMutation(t, f) })
	t.Run("CancelledCtxReturnsPromptly", func(t *testing.T) { preparerCancelledCtx(t, f) })
}

// ---------- subtests ----------

func preparerZeroInput(t *testing.T, f PreparerFactory) {
	t.Helper()
	p := f()
	defer recoverPanicAs(t, "Before(zero inputs)")
	_, _ = p.Before(context.Background(), agent.Identity{}, &agent.Request{}, agent.NewBoard())
}

func preparerNonNilBoard(t *testing.T, f PreparerFactory) {
	t.Helper()
	p := f()
	req := &agent.Request{TaskID: "t1"}
	next, err := p.Before(context.Background(), agent.Identity{RunID: "r1"}, req, agent.NewBoard())
	if err != nil {
		t.Skipf("preparer rejected the minimal inputs: %v", err)
	}
	if next == nil {
		t.Error("Before returned (nil, nil); a successful Before MUST return a non-nil board")
	}
}

func preparerFreshBoard(t *testing.T, f PreparerFactory) {
	t.Helper()
	p := f()
	req := &agent.Request{TaskID: "t1"}
	id := agent.Identity{RunID: "r1"}
	prev := agent.NewBoard()

	first, err := p.Before(context.Background(), id, req, prev)
	if err != nil {
		t.Skipf("preparer rejected the minimal inputs: %v", err)
	}
	if first == prev {
		t.Error("Before returned the input board; Preparers MUST return a fresh value each call (the engine mutates it in place)")
	}
	second, err := p.Before(context.Background(), id, req, prev)
	if err != nil {
		t.Skipf("second Before rejected the minimal inputs: %v", err)
	}
	if second == prev {
		t.Error("Before returned the input board; Preparers MUST return a fresh value each call")
	}
	if second == first {
		t.Error("two Before calls returned the same board pointer; each call MUST allocate a fresh value")
	}
}

func preparerNoMutation(t *testing.T, f PreparerFactory) {
	t.Helper()
	p := f()
	req := &agent.Request{TaskID: "t1"}
	id := agent.Identity{RunID: "r1"}
	prev := agent.NewBoard()
	prev.SetVar("keep", "me")
	before := prev.Clone()

	if _, err := p.Before(context.Background(), id, req, prev); err != nil {
		t.Skipf("preparer rejected the minimal inputs: %v", err)
	}
	if !reflect.DeepEqual(prev, before) {
		t.Errorf("Before mutated the previous board:\n  before: %+v\n  after : %+v", before, prev)
	}
}

func preparerCancelledCtx(t *testing.T, f PreparerFactory) {
	t.Helper()
	p := f()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		_, _ = p.Before(ctx, agent.Identity{}, &agent.Request{}, agent.NewBoard())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Before did not return within 2s of a cancelled ctx; Preparers MUST honour ctx cancellation")
	}
}

// CommitterFactory builds a fresh [agent.Committer] for each subtest.
type CommitterFactory func() agent.Committer

// CommitterSuite runs every contract probe that applies to the
// [agent.Committer] produced by f:
//
//   - zero inputs must not panic;
//   - Commit MUST NOT mutate the Request or Result it receives;
//   - Commit MUST honour ctx cancellation and return promptly.
func CommitterSuite(t *testing.T, f CommitterFactory) {
	t.Helper()

	t.Run("ZeroInputNoPanic", func(t *testing.T) { committerZeroInput(t, f) })
	t.Run("DoesNotMutateInputs", func(t *testing.T) { committerNoMutation(t, f) })
	t.Run("CancelledCtxReturnsPromptly", func(t *testing.T) { committerCancelledCtx(t, f) })
}

// ---------- subtests ----------

func committerZeroInput(t *testing.T, f CommitterFactory) {
	t.Helper()
	c := f()
	defer recoverPanicAs(t, "Commit(zero inputs)")
	_ = c.Commit(context.Background(), agent.Identity{}, &agent.Request{}, &agent.Result{})
}

func committerNoMutation(t *testing.T, f CommitterFactory) {
	t.Helper()
	c := f()
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleAssistant, "hi"))
	req := &agent.Request{TaskID: "t1", ContextID: "c1"}
	res := &agent.Result{
		TaskID:    "t1",
		RunID:     "r1",
		Status:    agent.StatusCompleted,
		Committed: true,
		State:     map[string]any{"k": "v"},
		LastBoard: board,
	}
	reqBefore := *req
	resBefore := *res

	_ = c.Commit(context.Background(), agent.Identity{AgentID: "a", RunID: "r1"}, req, res)

	if !reflect.DeepEqual(*req, reqBefore) {
		t.Errorf("Commit mutated *Request:\n  before: %+v\n  after : %+v", reqBefore, *req)
	}
	if !reflect.DeepEqual(*res, resBefore) {
		t.Errorf("Commit mutated *Result:\n  before: %+v\n  after : %+v", resBefore, *res)
	}
}

func committerCancelledCtx(t *testing.T, f CommitterFactory) {
	t.Helper()
	c := f()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		_ = c.Commit(ctx, agent.Identity{}, &agent.Request{}, &agent.Result{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Commit did not return within 2s of a cancelled ctx; Committers MUST honour ctx cancellation")
	}
}

// CommitViewFactory builds a fresh [agent.CommitViewProvider] for each
// subtest.
type CommitViewFactory func() agent.CommitViewProvider

// CommitViewProviderSuite runs every contract probe that applies to
// the [agent.CommitViewProvider] produced by f:
//
//   - zero inputs must not panic;
//   - a successful CommitView MUST carry a non-nil LastBoard (a nil
//     board with a nil error silently fails finalization);
//   - CommitView MUST NOT mutate the Request or Result it receives;
//   - CommitView MUST honour ctx cancellation and return promptly.
func CommitViewProviderSuite(t *testing.T, f CommitViewFactory) {
	t.Helper()

	t.Run("ZeroInputNoPanic", func(t *testing.T) { commitViewZeroInput(t, f) })
	t.Run("NonNilBoardOnSuccess", func(t *testing.T) { commitViewNonNilBoard(t, f) })
	t.Run("DoesNotMutateInputs", func(t *testing.T) { commitViewNoMutation(t, f) })
	t.Run("CancelledCtxReturnsPromptly", func(t *testing.T) { commitViewCancelledCtx(t, f) })
}

// ---------- subtests ----------

func commitViewZeroInput(t *testing.T, f CommitViewFactory) {
	t.Helper()
	p := f()
	defer recoverPanicAs(t, "CommitView(zero inputs)")
	_, _ = p.CommitView(context.Background(), agent.Identity{}, &agent.Request{}, &agent.Result{})
}

func commitViewNonNilBoard(t *testing.T, f CommitViewFactory) {
	t.Helper()
	p := f()
	req := &agent.Request{TaskID: "t1"}
	res := &agent.Result{Status: agent.StatusCompleted, LastBoard: agent.NewBoard()}
	view, err := p.CommitView(context.Background(), agent.Identity{RunID: "r1"}, req, res)
	if err != nil {
		t.Skipf("provider rejected the minimal inputs: %v", err)
	}
	if view.LastBoard == nil {
		t.Error("CommitView returned (CommitView{LastBoard: nil}, nil); a nil LastBoard silently fails finalization and must instead be surfaced as an error")
	}
}

func commitViewNoMutation(t *testing.T, f CommitViewFactory) {
	t.Helper()
	p := f()
	req := &agent.Request{TaskID: "t1", ContextID: "c1"}
	res := &agent.Result{
		TaskID:    "t1",
		RunID:     "r1",
		Status:    agent.StatusCompleted,
		Committed: true,
		State:     map[string]any{"k": "v"},
		LastBoard: agent.NewBoard(),
	}
	reqBefore := *req
	resBefore := *res

	_, _ = p.CommitView(context.Background(), agent.Identity{AgentID: "a", RunID: "r1"}, req, res)

	if !reflect.DeepEqual(*req, reqBefore) {
		t.Errorf("CommitView mutated *Request:\n  before: %+v\n  after : %+v", reqBefore, *req)
	}
	if !reflect.DeepEqual(*res, resBefore) {
		t.Errorf("CommitView mutated *Result:\n  before: %+v\n  after : %+v", resBefore, *res)
	}
}

func commitViewCancelledCtx(t *testing.T, f CommitViewFactory) {
	t.Helper()
	p := f()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		_, _ = p.CommitView(ctx, agent.Identity{}, &agent.Request{}, &agent.Result{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("CommitView did not return within 2s of a cancelled ctx; providers MUST honour ctx cancellation")
	}
}
