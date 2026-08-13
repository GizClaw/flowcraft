package agenttest

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
)

// RefereeFactory builds a fresh [agent.Referee] for each subtest.
// The suite invokes it once per case so subtests do not share
// decider state.
type RefereeFactory func() agent.Referee

// RefereeCapabilities lets a decider opt out of subtests that
// don't apply. Most deciders pass the zero value (= every subtest
// runs).
type RefereeCapabilities struct {
	// SkipMutationCheck is true when the decider intentionally
	// mutates *Result (extremely rare — the interface comment
	// says "MUST NOT mutate res"; this knob exists only for
	// deciders that document a deliberate breach for migration
	// purposes).
	SkipMutationCheck bool

	// SkipAllStatusProbe is true for deciders that legitimately
	// only handle a subset of statuses and panic on others. The
	// suite then skips the cross-status probe; the decider is
	// expected to ship its own focused unit tests.
	SkipAllStatusProbe bool
}

// RefereeSuite runs every applicable contract subtest against
// deciders produced by f.
func RefereeSuite(t *testing.T, f RefereeFactory, caps ...RefereeCapabilities) {
	t.Helper()
	c := RefereeCapabilities{}
	if len(caps) > 0 {
		c = caps[0]
	}

	t.Run("ZeroInputNoPanic", func(t *testing.T) { deciderZeroInput(t, f) })
	t.Run("ContextCancelTolerant", func(t *testing.T) { deciderCtxCancel(t, f) })
	if !c.SkipMutationCheck {
		t.Run("DoesNotMutateResult", func(t *testing.T) { deciderNoMutation(t, f) })
	}
	if !c.SkipAllStatusProbe {
		t.Run("HandlesEveryStatus", func(t *testing.T) { deciderAllStatuses(t, f) })
	}
	t.Run("ConcurrentSafe", func(t *testing.T) { deciderConcurrent(t, f) })
}

// ---------- subtests ----------

func deciderZeroInput(t *testing.T, f RefereeFactory) {
	t.Helper()
	d := f()
	defer recoverPanicAs(t, "After(zero inputs)")
	_, _ = d.After(context.Background(), agent.Identity{}, &agent.Request{}, &agent.Result{})
}

func deciderCtxCancel(t *testing.T, f RefereeFactory) {
	t.Helper()
	d := f()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	defer recoverPanicAs(t, "After(cancelled ctx)")
	// Either honour ctx.Err or return zero — both are valid; the
	// only failure mode is panic / hang.
	done := make(chan struct{})
	go func() {
		_, _ = d.After(ctx, agent.Identity{}, &agent.Request{}, &agent.Result{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("After did not return within 2s of a cancelled ctx; deciders that block on external work must select on ctx.Done()")
	}
}

func deciderNoMutation(t *testing.T, f RefereeFactory) {
	t.Helper()
	d := f()
	req := &agent.Request{TaskID: "task-1", ContextID: "ctx-1"}
	res := &agent.Result{
		TaskID:    "task-1",
		RunID:     "run-1",
		Status:    agent.StatusCompleted,
		Committed: true,
		State:     map[string]any{"k": "v"},
	}
	reqBefore := *req
	resBefore := *res
	stateBefore := map[string]any{}
	for k, v := range res.State {
		stateBefore[k] = v
	}

	_, _ = d.After(context.Background(), agent.Identity{AgentID: "a", RunID: "run-1"}, req, res)

	if !reflect.DeepEqual(*req, reqBefore) {
		t.Errorf("After mutated *Request:\n  before: %+v\n  after : %+v", reqBefore, *req)
	}
	if !reflect.DeepEqual(res.Messages, resBefore.Messages) ||
		res.Status != resBefore.Status ||
		res.Committed != resBefore.Committed ||
		res.RunID != resBefore.RunID {
		t.Errorf("After mutated *Result fields:\n  before: %+v\n  after : %+v", resBefore, *res)
	}
	if !reflect.DeepEqual(res.State, stateBefore) {
		t.Errorf("After mutated Result.State:\n  before: %+v\n  after : %+v", stateBefore, res.State)
	}
}

func deciderAllStatuses(t *testing.T, f RefereeFactory) {
	t.Helper()
	statuses := []agent.Status{
		agent.StatusCompleted,
		agent.StatusInterrupted,
		agent.StatusCanceled,
		agent.StatusFailed,
		agent.StatusAborted,
	}
	for _, st := range statuses {
		st := st
		t.Run(string(st), func(t *testing.T) {
			d := f()
			defer recoverPanicAs(t, "After(Status="+string(st)+")")
			_, _ = d.After(context.Background(),
				agent.Identity{AgentID: "a", RunID: "r"},
				&agent.Request{},
				&agent.Result{Status: st})
		})
	}
}

func deciderConcurrent(t *testing.T, f RefereeFactory) {
	t.Helper()
	d := f()
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("concurrent After panicked: %v — agent runtime invokes Referee chains sequentially today, but stateless deciders should remain safe under concurrent reuse across runs", r)
				}
			}()
			_, _ = d.After(context.Background(), agent.Identity{AgentID: "a", RunID: "r"}, &agent.Request{}, &agent.Result{Status: agent.StatusCompleted})
		}()
	}
	wg.Wait()
}

// recoverPanicAs converts a panic inside a probe into a t.Errorf so
// the test binary stays alive and the failure message names the
// offending method. Shared helper for both RefereeSuite and
// ObserverSuite.
func recoverPanicAs(t *testing.T, label string) {
	if r := recover(); r != nil {
		t.Errorf("%s panicked: %v — agent.Execute invokes this method unconditionally; a panic kills the run", label, r)
	}
}
