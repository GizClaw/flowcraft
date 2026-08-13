package agenttest

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// ResumerFactory builds a fresh [agent.Resumer] for each subtest so
// subtests do not share implementation state.
type ResumerFactory func() agent.Resumer

// ResumerSuite runs every contract probe that applies to the
// [agent.Resumer] produced by f. Resumer is the optional cheap,
// local-only admission probe an engine exposes ahead of resume:
//
//   - zero / malformed checkpoints must not panic, and any returned
//     error must be classified as Validation (wrong shape) or
//     NotAvailable (recognised but not resumable);
//   - an empty SpecVersion must never be rejected as a drift failure
//     (the checkpoint contract says empty means "skip drift check");
//   - the probe must not mutate the caller's checkpoint;
//   - it must be cheap (no I/O, no LLM calls) and safe for
//     concurrent use.
func ResumerSuite(t *testing.T, f ResumerFactory) {
	t.Helper()

	t.Run("ZeroInputNoPanic", func(t *testing.T) { resumerZeroInput(t, f) })
	t.Run("EmptySpecVersionNotDrift", func(t *testing.T) { resumerEmptySpecVersion(t, f) })
	t.Run("DoesNotMutateCheckpoint", func(t *testing.T) { resumerNoMutation(t, f) })
	t.Run("PromptReturn", func(t *testing.T) { resumerPromptReturn(t, f) })
	t.Run("ConcurrentSafe", func(t *testing.T) { resumerConcurrent(t, f) })
}

// ---------- subtests ----------

func resumerZeroInput(t *testing.T, f ResumerFactory) {
	t.Helper()
	r := f()
	defer recoverPanicAs(t, "CanResume(zero checkpoint)")
	err := r.CanResume(agent.Checkpoint{})
	if err != nil && !errdefs.IsValidation(err) && !errdefs.IsNotAvailable(err) {
		t.Errorf("CanResume(zero checkpoint) = %v; must be nil or classified Validation/NotAvailable", err)
	}
}

// resumerEmptySpecVersion verifies the documented "empty SpecVersion
// means skip the drift check" rule: a Resumer must never return
// NotAvailable for a checkpoint whose SpecVersion is empty, whatever
// its own current version is.
func resumerEmptySpecVersion(t *testing.T, f ResumerFactory) {
	t.Helper()
	r := f()
	cp := agent.Checkpoint{
		ExecID: "run-1",
		Board:  &agent.BoardSnapshot{},
	}
	defer recoverPanicAs(t, "CanResume(empty SpecVersion)")
	err := r.CanResume(cp)
	if errdefs.IsNotAvailable(err) {
		t.Fatalf("CanResume rejected an empty SpecVersion as NotAvailable; empty means 'skip drift check': %v", err)
	}
	if err != nil && !errdefs.IsValidation(err) {
		t.Errorf("CanResume(empty SpecVersion) = %v; non-drift rejections must be classified Validation", err)
	}
}

// resumerNoMutation asserts the probe treats the checkpoint as
// caller-owned: rejections (or acceptances) must not modify the
// shared Steps / Board / Payload / Attributes state.
func resumerNoMutation(t *testing.T, f ResumerFactory) {
	t.Helper()
	r := f()
	cp := agent.Checkpoint{
		ExecID:            "run-1",
		Steps:             []string{"wave-1", "wave-2"},
		Iteration:         3,
		Board:             &agent.BoardSnapshot{Vars: map[string]any{"x": float64(1)}},
		Payload:           []byte(`{"task_id":"t1"}`),
		Attributes:        map[string]string{"tenant": "acme"},
		Timestamp:         time.Unix(1_700_000_000, 0),
		OriginalStartedAt: time.Unix(1_700_000_000, 0),
		SpecVersion:       "v1",
	}
	before := cp.Clone()
	defer recoverPanicAs(t, "CanResume(mutation probe)")
	_ = r.CanResume(cp)
	if !reflect.DeepEqual(cp, before) {
		t.Errorf("CanResume mutated its checkpoint:\n  before: %+v\n  after : %+v", before, cp)
	}
}

// resumerPromptReturn bounds CanResume so an implementation that
// performs I/O / LLM calls against the contract ("MUST be cheap")
// fails loudly instead of hanging the suite.
func resumerPromptReturn(t *testing.T, f ResumerFactory) {
	t.Helper()
	r := f()
	cp := agent.Checkpoint{ExecID: "run-1", Board: &agent.BoardSnapshot{}}
	done := make(chan struct{})
	go func() {
		_ = r.CanResume(cp)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("CanResume did not return within 2s; Resumer probes MUST be cheap (no I/O, no LLM calls)")
	}
}

func resumerConcurrent(t *testing.T, f ResumerFactory) {
	t.Helper()
	r := f()
	cp := agent.Checkpoint{ExecID: "run-1", Board: &agent.BoardSnapshot{}}
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("concurrent CanResume panicked: %v", r)
				}
			}()
			_ = r.CanResume(cp)
		}()
	}
	wg.Wait()
}
