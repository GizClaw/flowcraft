package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/message"
)

// ---------- Observer internals ----------

// composeObservers / multiObserver / safeRun live in the same package,
// so these tests sit in the internal test target.

func TestComposeObservers_NilSliceReturnsNil(t *testing.T) {
	if got := composeObservers(nil); got != nil {
		t.Errorf("composeObservers(nil) = %v, want nil", got)
	}
}

func TestComposeObservers_AllNilReturnsNil(t *testing.T) {
	if got := composeObservers([]Observer{nil, nil}); got != nil {
		t.Errorf("composeObservers(all nil) = %v, want nil", got)
	}
}

func TestComposeObservers_SingleEntry(t *testing.T) {
	rec := &captureObs{}
	obs := composeObservers([]Observer{rec})
	if obs == nil {
		t.Fatal("composeObservers should return non-nil for one observer")
	}

	obs.OnRunStart(context.Background(), Identity{RunID: "r"}, &Request{})
	if rec.startCalls != 1 {
		t.Errorf("OnRunStart fan-out failed; calls=%d", rec.startCalls)
	}
}

func TestComposeObservers_FansOutInOrder(t *testing.T) {
	var hits []string
	var mu sync.Mutex
	mark := func(name string) Observer {
		return &recOrder{onStart: func() {
			mu.Lock()
			hits = append(hits, name)
			mu.Unlock()
		}}
	}

	obs := composeObservers([]Observer{mark("a"), nil, mark("b"), mark("c")})
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
	obs := composeObservers([]Observer{
		&panicAll{},
		&recOrder{onStart: func() { firedAfter = true }},
	})

	obs.OnRunStart(context.Background(), Identity{}, &Request{})

	if !firedAfter {
		t.Error("subsequent observer must still fire after a peer panicked")
	}
}

func TestBaseObserver_NoOpsAreUsable(t *testing.T) {
	var b BaseObserver
	b.OnRunStart(context.Background(), Identity{}, &Request{})
	b.OnInterrupt(context.Background(), Identity{}, Interrupt{})
	b.OnRunEnd(context.Background(), Identity{}, &Result{})
}

// captureObs records call counts on every method. Lives next to the
// other internal observer-test helpers to avoid exposing it in
// agent_test.go.
type captureObs struct {
	BaseObserver
	startCalls     int
	interruptCalls int
	endCalls       int
}

func (c *captureObs) OnRunStart(context.Context, Identity, *Request)   { c.startCalls++ }
func (c *captureObs) OnInterrupt(context.Context, Identity, Interrupt) { c.interruptCalls++ }
func (c *captureObs) OnRunEnd(context.Context, Identity, *Result)      { c.endCalls++ }

type recOrder struct {
	BaseObserver
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

// ---------- Preparer internals ----------

// Tests live in the internal "agent" package because they probe
// defaultPreparer, which is unexported. Other agent_test.go files use
// the public API via "agent_test" — that boundary is intentional.

func TestSeedBoard_AppendsRequestMessage(t *testing.T) {
	req := &Request{Message: message.NewTextMessage(message.RoleUser, "hi")}

	b, err := seedBoard(context.Background(), Identity{}, req, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := b.Channel(MainChannel)
	if len(got) != 1 || got[0].Content.Text() != "hi" {
		t.Errorf("MainChannel = %+v, want [hi]", got)
	}
}

func TestSeedBoard_CopiesInputsToVars(t *testing.T) {
	req := &Request{
		Message: message.NewTextMessage(message.RoleUser, "hi"),
		Inputs:  map[string]any{"a": 1, "b": "two"},
	}

	b, err := seedBoard(context.Background(), Identity{}, req, nil)
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

func TestSeedBoard_FreshBoardEachCall(t *testing.T) {
	req := &Request{Message: message.NewTextMessage(message.RoleUser, "hi")}

	b1, _ := seedBoard(context.Background(), Identity{}, req, nil)
	b2, _ := seedBoard(context.Background(), Identity{}, req, nil)

	if b1 == b2 {
		t.Error("seedBoard must return a fresh Board each call")
	}
}

func TestPreparerFunc_Adapts(t *testing.T) {
	called := false
	f := PreparerFunc(func(_ context.Context, info Identity, req *Request, prev *Board) (*Board, error) {
		called = true
		if info.RunID != "r-1" {
			t.Errorf("Identity.RunID = %q, want r-1", info.RunID)
		}
		if req.Message.Content.Text() != "hello" {
			t.Errorf("req.Message = %q, want hello", req.Message.Content.Text())
		}
		if prev == nil {
			t.Error("prev must not be nil; chain always seeds before invoking a Preparer")
		}
		return NewBoard(), nil
	})

	_, err := f.Before(context.Background(),
		Identity{RunID: "r-1"},
		&Request{Message: message.NewTextMessage(message.RoleUser, "hello")},
		NewBoard(),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("PreparerFunc.Before did not invoke the wrapped function")
	}
}

func TestPreparerFunc_PropagatesError(t *testing.T) {
	boom := errors.New("boom")
	f := PreparerFunc(func(context.Context, Identity, *Request, *Board) (*Board, error) {
		return nil, boom
	})

	b, err := f.Before(context.Background(), Identity{}, &Request{}, NewBoard())
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want %v", err, boom)
	}
	if b != nil {
		t.Errorf("board should be nil on error; got %+v", b)
	}
}

// ---------- Referee internals ----------

// Internal-package tests for runReferee / Decision merging.
// runReferee is unexported so these stay in package agent.

type stubDecider struct {
	dec Decision
	err error
}

func (s stubDecider) After(context.Context, Identity, *Request, *Result) (Decision, error) {
	return s.dec, s.err
}

func TestComposeReferees_BoolsORed(t *testing.T) {
	got, err := composeReferees(context.Background(), Identity{}, &Request{}, &Result{},
		[]Referee{
			stubDecider{dec: Decision{DiscardOutput: true}},
			stubDecider{dec: Decision{Revise: true}},
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.DiscardOutput || !got.Revise {
		t.Errorf("OR-merge over booleans failed: %+v", got)
	}
}

func TestComposeReferees_FirstNonEmptyReasonWins(t *testing.T) {
	got, err := composeReferees(context.Background(), Identity{}, &Request{}, &Result{},
		[]Referee{
			stubDecider{dec: Decision{Reason: "first"}},
			stubDecider{dec: Decision{Reason: "second"}},
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Reason != "first" {
		t.Errorf("Reason = %q, want %q", got.Reason, "first")
	}

	got2, _ := composeReferees(context.Background(), Identity{}, &Request{}, &Result{},
		[]Referee{
			stubDecider{dec: Decision{}},
			stubDecider{dec: Decision{Reason: "second"}},
		})
	if got2.Reason != "second" {
		t.Errorf("merge into empty Reason = %q, want %q", got2.Reason, "second")
	}
}

func TestComposeReferees_NilEntriesSkipped(t *testing.T) {
	got, err := composeReferees(context.Background(), Identity{}, &Request{}, &Result{},
		[]Referee{nil, stubDecider{dec: Decision{Reason: "ok"}}, nil})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Reason != "ok" {
		t.Errorf("Reason = %q, want %q", got.Reason, "ok")
	}
}

func TestComposeReferees_FirstErrorShortCircuits(t *testing.T) {
	boom := errors.New("decider boom")
	called := 0
	d2 := stubDecider{dec: Decision{Reason: "should-not-merge"}}
	_ = d2 // silence unused warning; test is about composeReferees short-circuit

	_, err := composeReferees(context.Background(), Identity{}, &Request{}, &Result{},
		[]Referee{stubDecider{err: boom}, d2})
	if !errors.Is(err, boom) {
		t.Errorf("expected boom; got %v", err)
	}
	if called != 0 {
		t.Errorf("subsequent deciders ran after error; called=%d", called)
	}
}

func TestComposeReferees_AccumulatesAcrossReferees(t *testing.T) {
	got, err := composeReferees(context.Background(), Identity{}, &Request{}, &Result{},
		[]Referee{
			stubDecider{dec: Decision{Reason: "a"}},
			stubDecider{dec: Decision{DiscardOutput: true}},
			stubDecider{dec: Decision{Revise: true}},
		})
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

func TestBaseReferee_ZeroValueDecision(t *testing.T) {
	dec, err := BaseReferee{}.After(context.Background(), Identity{}, &Request{}, &Result{})
	if err != nil {
		t.Errorf("BaseReferee returned error: %v", err)
	}
	if (dec != Decision{}) {
		t.Errorf("BaseReferee returned non-zero decision: %+v", dec)
	}
}

type stubFn func() (Decision, error)

func (f stubFn) After(context.Context, Identity, *Request, *Result) (Decision, error) {
	return f()
}
