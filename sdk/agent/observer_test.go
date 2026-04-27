package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
)

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

	obs.OnRunStart(context.Background(), RunInfo{RunID: "r"}, &Request{})
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
	obs.OnRunStart(context.Background(), RunInfo{}, &Request{})

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

	obs.OnRunStart(context.Background(), RunInfo{}, &Request{})

	if !firedAfter {
		t.Error("subsequent observer must still fire after a peer panicked")
	}
}

func TestBaseObserver_NoOpsAreUsable(t *testing.T) {
	var b BaseObserver
	b.OnRunStart(context.Background(), RunInfo{}, &Request{})
	b.OnInterrupt(context.Background(), RunInfo{}, engine.Interrupt{})
	b.OnRunEnd(context.Background(), RunInfo{}, &Result{})
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

func (c *captureObs) OnRunStart(context.Context, RunInfo, *Request)          { c.startCalls++ }
func (c *captureObs) OnInterrupt(context.Context, RunInfo, engine.Interrupt) { c.interruptCalls++ }
func (c *captureObs) OnRunEnd(context.Context, RunInfo, *Result)             { c.endCalls++ }

type recOrder struct {
	BaseObserver
	onStart func()
}

func (r *recOrder) OnRunStart(context.Context, RunInfo, *Request) {
	if r.onStart != nil {
		r.onStart()
	}
}

type panicAll struct{}

func (panicAll) OnRunStart(context.Context, RunInfo, *Request)          { panic("boom") }
func (panicAll) OnInterrupt(context.Context, RunInfo, engine.Interrupt) { panic("boom") }
func (panicAll) OnRunEnd(context.Context, RunInfo, *Result)             { panic("boom") }
