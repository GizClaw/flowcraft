package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

// ---------- Hook internals ----------

// composeHooks / multiObserver / safeRun live in the same package,
// so these tests sit in the internal test target.

func TestComposeObservers_NilSliceReturnsNil(t *testing.T) {
	if got := composeHooks(nil); got != nil {
		t.Errorf("composeHooks(nil) = %v, want nil", got)
	}
}

func TestComposeObservers_AllNilReturnsNil(t *testing.T) {
	if got := composeHooks([]Hook{nil, nil}); got != nil {
		t.Errorf("composeHooks(all nil) = %v, want nil", got)
	}
}

func TestComposeObservers_SingleEntry(t *testing.T) {
	rec := &captureObs{}
	obs := composeHooks([]Hook{rec})
	if obs == nil {
		t.Fatal("composeHooks should return non-nil for one observer")
	}

	obs.OnRunStart(context.Background(), Identity{RunID: "r"}, &Request{})
	if rec.startCalls != 1 {
		t.Errorf("OnRunStart fan-out failed; calls=%d", rec.startCalls)
	}
}

func TestComposeObservers_FansOutInOrder(t *testing.T) {
	var hits []string
	var mu sync.Mutex
	mark := func(name string) Hook {
		return &recOrder{onStart: func() {
			mu.Lock()
			hits = append(hits, name)
			mu.Unlock()
		}}
	}

	obs := composeHooks([]Hook{mark("a"), nil, mark("b"), mark("c")})
	obs.OnRunStart(context.Background(), Identity{}, &Request{})

	got := strings.Join(hits, ",")
	if got != "a,b,c" {
		t.Errorf("fan-out order = %q, want %q", got, "a,b,c")
	}
}

func TestSafeRun_RecoversPanic(t *testing.T) {
	// safeRun must NOT propagate the panic.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("safeRun let panic escape: %v", r)
		}
	}()
	safeRun(func() { panic("boom") })
}

func TestMultiObserver_OnePanic_NextStillRuns(t *testing.T) {
	var firedAfter bool
	obs := composeHooks([]Hook{
		&panicAll{},
		&recOrder{onStart: func() { firedAfter = true }},
	})

	obs.OnRunStart(context.Background(), Identity{}, &Request{})

	if !firedAfter {
		t.Error("subsequent observer must still fire after a peer panicked")
	}
}

func TestBaseObserver_NoOpsAreUsable(t *testing.T) {
	var b BaseHook
	b.OnRunStart(context.Background(), Identity{}, &Request{})
	b.OnInterrupt(context.Background(), Identity{}, Interrupt{})
	b.OnRunEnd(context.Background(), Identity{}, &Result{})
}

// captureObs records call counts on every method. Lives next to the
// other internal observer-test helpers to avoid exposing it in
// agent_test.go.
type captureObs struct {
	BaseHook
	startCalls     int
	interruptCalls int
	endCalls       int
}

func (c *captureObs) OnRunStart(context.Context, Identity, *Request)   { c.startCalls++ }
func (c *captureObs) OnInterrupt(context.Context, Identity, Interrupt) { c.interruptCalls++ }
func (c *captureObs) OnRunEnd(context.Context, Identity, *Result)      { c.endCalls++ }

type recOrder struct {
	BaseHook
	onStart func()
}

func (r *recOrder) OnRunStart(context.Context, Identity, *Request) {
	if r.onStart != nil {
		r.onStart()
	}
}

type panicAll struct{}

func (panicAll) OnRunStart(context.Context, Identity, *Request)      { panic("boom") }
func (panicAll) OnInterrupt(context.Context, Identity, Interrupt)    { panic("boom") }
func (panicAll) OnRunRevise(context.Context, Identity, *Result, int) { panic("boom") }
func (panicAll) OnRunEnd(context.Context, Identity, *Result)         { panic("boom") }

// ---------- BeforeExecute internals ----------

// Tests live in the internal "agent" package because they probe
// defaultBefore, which is unexported. Other agent_test.go files use
// the public API via "agent_test" — that boundary is intentional.

func TestDefaultSeeder_AppendsRequestMessage(t *testing.T) {
	req := &Request{Message: inference.NewTextMessage(inference.RoleUser, "hi")}

	b, err := defaultBefore{}.Before(context.Background(), Identity{}, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := b.Channel(MainChannel)
	if len(got) != 1 || got[0].Content.Text() != "hi" {
		t.Errorf("MainChannel = %+v, want [hi]", got)
	}
}

func TestDefaultSeeder_CopiesInputsToVars(t *testing.T) {
	req := &Request{
		Message: inference.NewTextMessage(inference.RoleUser, "hi"),
		Inputs:  map[string]any{"a": 1, "b": "two"},
	}

	b, err := defaultBefore{}.Before(context.Background(), Identity{}, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, _ := b.GetVar("a"); v != 1 {
		t.Errorf("vars[a] = %v, want 1", v)
	}
	if v, _ := b.GetVar("b"); v != "two" {
		t.Errorf("vars[b] = %v, want two", v)
	}
}

func TestDefaultSeeder_FreshBoardEachCall(t *testing.T) {
	req := &Request{Message: inference.NewTextMessage(inference.RoleUser, "hi")}

	b1, _ := defaultBefore{}.Before(context.Background(), Identity{}, req)
	b2, _ := defaultBefore{}.Before(context.Background(), Identity{}, req)

	if b1 == b2 {
		t.Error("defaultBefore must return a fresh Board each call")
	}
}

func TestBoardSeederFunc_Adapts(t *testing.T) {
	called := false
	f := BeforeExecuteFunc(func(_ context.Context, info Identity, req *Request) (*Board, error) {
		called = true
		if info.RunID != "r-1" {
			t.Errorf("Identity.RunID = %q, want r-1", info.RunID)
		}
		if req.Message.Content.Text() != "hello" {
			t.Errorf("req.Message = %q, want hello", req.Message.Content.Text())
		}
		return NewBoard(), nil
	})

	_, err := f.Before(context.Background(),
		Identity{RunID: "r-1"},
		&Request{Message: inference.NewTextMessage(inference.RoleUser, "hello")},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("BeforeExecuteFunc.Before did not invoke the wrapped function")
	}
}

func TestBoardSeederFunc_PropagatesError(t *testing.T) {
	boom := errors.New("boom")
	f := BeforeExecuteFunc(func(context.Context, Identity, *Request) (*Board, error) {
		return nil, boom
	})

	b, err := f.Before(context.Background(), Identity{}, &Request{})
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want %v", err, boom)
	}
	if b != nil {
		t.Errorf("board should be nil on error; got %+v", b)
	}
}

// ---------- AfterExecute internals ----------

// Internal-package tests for runAfterExecute / Decision merging.
// runAfterExecute is unexported so these stay in package agent.

type stubDecider struct {
	dec Decision
	err error
}

func (s stubDecider) After(context.Context, Identity, *Request, *Result) (Decision, error) {
	return s.dec, s.err
}

func TestFinalizeDecision_Merge_BoolsORed(t *testing.T) {
	a := Decision{DiscardOutput: true}
	b := Decision{Revise: true}

	got := a.merge(b)
	if !got.DiscardOutput || !got.Revise {
		t.Errorf("merge OR over bools failed: %+v", got)
	}
}

func TestFinalizeDecision_Merge_FirstNonEmptyReasonWins(t *testing.T) {
	first := Decision{Reason: "first"}
	second := Decision{Reason: "second"}

	got := first.merge(second)
	if got.Reason != "first" {
		t.Errorf("Reason = %q, want %q", got.Reason, "first")
	}

	got2 := Decision{}.merge(second)
	if got2.Reason != "second" {
		t.Errorf("merge into empty Reason = %q, want %q", got2.Reason, "second")
	}
}

func TestRunDeciders_NilEntriesSkipped(t *testing.T) {
	got, err := runAfterExecute(context.Background(),
		[]AfterExecute{nil, stubDecider{dec: Decision{Reason: "ok"}}, nil},
		Identity{}, &Request{}, &Result{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Reason != "ok" {
		t.Errorf("Reason = %q, want %q", got.Reason, "ok")
	}
}

func TestRunDeciders_FirstErrorShortCircuits(t *testing.T) {
	boom := errors.New("decider boom")
	called := 0
	d2 := stubFn(func() (Decision, error) {
		called++
		return Decision{Reason: "should-not-merge"}, nil
	})

	_, err := runAfterExecute(context.Background(),
		[]AfterExecute{stubDecider{err: boom}, d2},
		Identity{}, &Request{}, &Result{})
	if !errors.Is(err, boom) {
		t.Errorf("expected boom; got %v", err)
	}
	if called != 0 {
		t.Errorf("subsequent deciders ran after error; called=%d", called)
	}
}

func TestRunDeciders_AccumulatesAcrossDeciders(t *testing.T) {
	got, err := runAfterExecute(context.Background(),
		[]AfterExecute{
			stubDecider{dec: Decision{Reason: "a"}},
			stubDecider{dec: Decision{DiscardOutput: true}},
			stubDecider{dec: Decision{Revise: true}},
		},
		Identity{}, &Request{}, &Result{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.DiscardOutput || !got.Revise {
		t.Errorf("OR fold lost a bool: %+v", got)
	}
	if got.Reason != "a" {
		t.Errorf("first non-empty Reason should win: %q", got.Reason)
	}
}

func TestBaseDecider_ZeroValueDecision(t *testing.T) {
	dec, err := BaseAfterExecute{}.After(context.Background(), Identity{}, &Request{}, &Result{})
	if err != nil {
		t.Errorf("BaseAfterExecute returned error: %v", err)
	}
	if (dec != Decision{}) {
		t.Errorf("BaseAfterExecute returned non-zero decision: %+v", dec)
	}
}

type stubFn func() (Decision, error)

func (f stubFn) After(context.Context, Identity, *Request, *Result) (Decision, error) {
	return f()
}
