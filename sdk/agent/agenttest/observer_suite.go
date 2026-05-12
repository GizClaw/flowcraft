package agenttest

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
)

// ObserverFactory builds a fresh [agent.Observer] for each subtest.
type ObserverFactory func() agent.Observer

// ObserverCapabilities lets observers opt out of subtests that
// don't apply. Most observers pass the zero value.
type ObserverCapabilities struct {
	// SkipMutationCheck is true for observers that intentionally
	// mutate the *Request / *Result they receive. The interface
	// godoc says "MUST treat as read-only"; this knob exists only
	// for the rare migration shim that documents a deliberate
	// breach.
	SkipMutationCheck bool

	// SkipPromptReturnCheck is true for observers that legitimately
	// block (e.g. a synchronous flush observer the caller knows
	// about). The interface godoc says "blocking blocks the run",
	// so the suite enforces a 2s upper bound by default.
	SkipPromptReturnCheck bool
}

// ObserverSuite runs every applicable contract subtest against
// observers produced by f.
func ObserverSuite(t *testing.T, f ObserverFactory, caps ...ObserverCapabilities) {
	t.Helper()
	c := ObserverCapabilities{}
	if len(caps) > 0 {
		c = caps[0]
	}

	t.Run("ZeroInputsNoPanic", func(t *testing.T) { observerZeroInputs(t, f) })
	if !c.SkipMutationCheck {
		t.Run("DoesNotMutateInputs", func(t *testing.T) { observerNoMutation(t, f) })
	}
	if !c.SkipPromptReturnCheck {
		t.Run("HooksReturnPromptly", func(t *testing.T) { observerPromptReturn(t, f) })
	}
	t.Run("ConcurrentSafe", func(t *testing.T) { observerConcurrent(t, f) })
}

// ---------- subtests ----------

func observerZeroInputs(t *testing.T, f ObserverFactory) {
	t.Helper()
	o := f()
	ctx := context.Background()
	info := agent.RunInfo{}

	probe := func(label string, fn func()) {
		t.Helper()
		defer recoverPanicAs(t, label)
		fn()
	}
	probe("OnRunStart(zero)", func() { o.OnRunStart(ctx, info, &agent.Request{}) })
	probe("OnInterrupt(zero)", func() { o.OnInterrupt(ctx, info, engine.Interrupt{}) })
	probe("OnRunRevise(zero)", func() { o.OnRunRevise(ctx, info, &agent.Result{}, 2) })
	probe("OnRunEnd(zero)", func() { o.OnRunEnd(ctx, info, &agent.Result{}) })
}

func observerNoMutation(t *testing.T, f ObserverFactory) {
	t.Helper()
	o := f()
	ctx := context.Background()
	info := agent.RunInfo{AgentID: "a", RunID: "r"}

	check := func(label string, before, after any) {
		t.Helper()
		if !reflect.DeepEqual(before, after) {
			t.Errorf("%s mutated input:\n  before: %+v\n  after : %+v", label, before, after)
		}
	}

	req := &agent.Request{TaskID: "t1"}
	reqCopy := *req
	o.OnRunStart(ctx, info, req)
	check("OnRunStart", reqCopy, *req)

	intr := engine.Interrupt{Cause: engine.CauseUserInput, Detail: "d"}
	intrCopy := intr
	o.OnInterrupt(ctx, info, intr)
	check("OnInterrupt", intrCopy, intr)

	prev := &agent.Result{Status: agent.StatusFailed, Attempts: 1}
	prevCopy := *prev
	o.OnRunRevise(ctx, info, prev, 2)
	check("OnRunRevise(prev)", prevCopy, *prev)

	res := &agent.Result{Status: agent.StatusCompleted, Committed: true}
	resCopy := *res
	o.OnRunEnd(ctx, info, res)
	check("OnRunEnd(res)", resCopy, *res)
}

func observerPromptReturn(t *testing.T, f ObserverFactory) {
	t.Helper()
	o := f()
	ctx := context.Background()
	info := agent.RunInfo{}

	timed := func(label string, fn func()) {
		t.Helper()
		done := make(chan struct{})
		go func() { fn(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("%s did not return within 2s — observer methods MUST be non-blocking (interface comment)", label)
		}
	}
	timed("OnRunStart", func() { o.OnRunStart(ctx, info, &agent.Request{}) })
	timed("OnInterrupt", func() { o.OnInterrupt(ctx, info, engine.Interrupt{}) })
	timed("OnRunRevise", func() { o.OnRunRevise(ctx, info, &agent.Result{}, 2) })
	timed("OnRunEnd", func() { o.OnRunEnd(ctx, info, &agent.Result{}) })
}

func observerConcurrent(t *testing.T, f ObserverFactory) {
	t.Helper()
	o := f()
	const n = 16
	var wg sync.WaitGroup
	ctx := context.Background()
	info := agent.RunInfo{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("concurrent observer dispatch panicked: %v — agent.Run invokes a single Observer instance sequentially per run, but the same observer is reused across runs and must be safe for concurrent use", r)
				}
			}()
			o.OnRunStart(ctx, info, &agent.Request{})
			o.OnRunEnd(ctx, info, &agent.Result{Status: agent.StatusCompleted})
		}()
	}
	wg.Wait()
}
