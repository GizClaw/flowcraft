//go:build windows

package sessionkit

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// The message codes are spelled out rather than imported, so the mapping from a
// code to the cap it reports is the one thing worth pinning: a wrong number is
// how a process-limit message became a cpu-budget verdict.
func TestCapForMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		qty  uint32
		want string
	}{
		{"job memory limit", jobObjectMsgJobMemoryLimit, "memory"},
		{"end of job time", jobObjectMsgEndOfJobTime, "cpu"},
		{"end of process time", jobObjectMsgEndOfProcessTime, "cpu"},
		// 3 is ACTIVE_PROCESS_LIMIT, which this file used to call
		// END_OF_PROCESS_TIME. It reports no cap, so it must not classify as one.
		{"active process limit", 3, ""},
		{"unknown", 99, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := capForMessage(tc.qty); got != tc.want {
				t.Errorf("capForMessage(%d) = %q, want %q", tc.qty, got, tc.want)
			}
		})
	}
}

// The record and the wake-up are what classification and the kill depend on, and
// both arrive from a goroutine: the first cap wins, and capped is released once.
func TestMarkCapRecordsTheFirstCap(t *testing.T) {
	j := &job{capped: make(chan struct{})}
	j.markCap("memory")
	j.markCap("cpu")
	if got := j.budgetCap(); got != "memory" {
		t.Errorf("budgetCap = %q, want the first cap (memory)", got)
	}
	select {
	case <-j.capped:
	default:
		t.Fatal("capped was not released")
	}
}

// A cap can be reported at the moment the session ends, when the cap goroutine is
// a scheduler tick behind the release. Terminating then must be a no-op rather
// than a call through a handle that may already belong to another session's job.
func TestTerminateAfterCloseIsANoOp(t *testing.T) {
	j, err := createJob(policy.Resources{MemoryBytes: 1 << 20}, time.Second)
	if err != nil {
		t.Fatalf("createJob: %v", err)
	}
	if err := j.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := j.terminate(); err != nil {
		t.Fatalf("terminate after close = %v, want nil", err)
	}
}

// A session with a memory budget starts the sampler; ending the session ends it,
// and an unexceeded budget records no cap.
func TestEnforceCapsStopsWithTheSession(t *testing.T) {
	j, err := createJob(policy.Resources{MemoryBytes: 1 << 20}, time.Second)
	if err != nil {
		t.Fatalf("createJob: %v", err)
	}
	defer func() { _ = j.close() }()
	done := make(chan struct{})
	j.enforceCaps(done)
	time.Sleep(3 * jobMemorySampleInterval)
	close(done)
	if got := j.budgetCap(); got != "" {
		t.Errorf("budgetCap = %q for a session under its budget, want empty", got)
	}
}

// A sampler that stops working ends the session rather than leaving it unguarded:
// the budget the caller asked for is no longer being measured, and the capability
// still claims it. The verdict is unenforceable, not a budget breach, and the kill
// has to actually happen — a verdict with no kill is the failure this exists to
// prevent.
func TestSamplerFailureFailsClosedAsUnenforceable(t *testing.T) {
	previousInterval := jobMemorySampleInterval
	jobMemorySampleInterval = time.Millisecond
	t.Cleanup(func() { jobMemorySampleInterval = previousInterval })

	j, err := createJob(policy.Resources{MemoryBytes: 1 << 20}, time.Minute)
	if err != nil {
		t.Fatalf("createJob: %v", err)
	}
	defer func() { _ = j.close() }()
	// The kill and the sampler both run on the sampler goroutine, so the counters
	// they touch are atomic: this package's own tests run under -race on the
	// Windows lane.
	var kills, samples atomic.Int64
	j.kill = func() { kills.Add(1) }
	j.sample = func() (uintptr, error) { samples.Add(1); return 0, errors.New("query refused") }

	done := make(chan struct{})
	defer close(done)
	j.enforceCaps(done)

	deadline := time.Now().Add(2 * time.Second)
	for j.unenforceable() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := j.unenforceable(); err == nil {
		t.Fatalf("still sampling after %d consecutive failures", maxCapSampleFailures)
	}
	if got := kills.Load(); got != 1 {
		t.Errorf("kills = %d, want exactly 1: the verdict without the kill leaves the child running", got)
	}
	if cap := j.budgetCap(); cap != "" {
		t.Errorf("budgetCap = %q, want empty: nothing was measured", cap)
	}
	// The goroutine stopped: no further samples after it gave up.
	settled := samples.Load()
	time.Sleep(5 * jobMemorySampleInterval)
	if got := samples.Load(); got != settled {
		t.Errorf("sampled %d more times after giving up; the watcher must stop", got-settled)
	}
}

// One flaky sample must not kill an innocent session; a good one resets the count.
func TestSamplerFailureResetsOnASuccessfulSample(t *testing.T) {
	previous := jobMemorySampleInterval
	jobMemorySampleInterval = time.Millisecond
	t.Cleanup(func() { jobMemorySampleInterval = previous })

	j, err := createJob(policy.Resources{MemoryBytes: 1 << 20}, time.Minute)
	if err != nil {
		t.Fatalf("createJob: %v", err)
	}
	defer func() { _ = j.close() }()
	var calls atomic.Int64
	j.sample = func() (uintptr, error) {
		if calls.Add(1)%2 == 0 {
			return 0, nil // every second sample works
		}
		return 0, errors.New("transient")
	}

	done := make(chan struct{})
	defer close(done)
	j.enforceCaps(done)
	time.Sleep(20 * maxCapSampleFailures * jobMemorySampleInterval)
	if got := calls.Load(); got < 2*maxCapSampleFailures {
		t.Fatalf("only %d samples in the window: the test proved nothing", got)
	}
	if err := j.unenforceable(); err != nil {
		t.Fatalf("declared unenforceable on alternating samples: %v", err)
	}
}

// A released job cannot be sampled, and that must not count as a failure: the
// sampler wakes between release and the session's done channel closing.
func TestSamplerIgnoresAReleasedJob(t *testing.T) {
	j, err := createJob(policy.Resources{MemoryBytes: 1 << 20}, time.Minute)
	if err != nil {
		t.Fatalf("createJob: %v", err)
	}
	j.sample = func() (uintptr, error) {
		t.Error("sampled a released job")
		return 0, nil
	}
	if err := j.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if over, err := j.sampleCaps(); over || err != nil {
		t.Fatalf("sampleCaps on a released job = (%v, %v), want (false, nil)", over, err)
	}
	if err := j.unenforceable(); err != nil {
		t.Errorf("unenforceable = %v after a released sample, want nil", err)
	}
}

// A breach is attributed from the job's accounting, so a lost verdict channel
// still reports a budget breach rather than a broken guarantee.
func TestClassifyCPUBreachSurvivesALostReport(t *testing.T) {
	j := &job{limits: policy.Resources{CPUMillicores: 500}, capped: make(chan struct{}), cpuLimit: 10_000_000}
	j.accounting = func() (int64, error) { return j.cpuLimit, nil }
	j.notify = &jobNotifier{done: make(chan struct{})}
	j.notify.recordStop(errors.New("completion port closed"))
	exit, err := classifyExit(&session{}, j, &ttyExitError{code: 1})
	if exit.Reason != sandbox.SessionBudgetExceeded {
		t.Errorf("Reason = %v, want budget exceeded", exit.Reason)
	}
	if !errdefs.IsBudgetExceeded(err) || errdefs.IsNotAvailable(err) {
		t.Errorf("err = %v, want BudgetExceeded and not NotAvailable", err)
	}
}

// Nothing can attribute the exit when the accounting is unreadable *and* the
// report is gone: that is a broken guarantee, and it must say so.
func TestClassifyCPUVerdictLossIsUnenforceable(t *testing.T) {
	j := &job{limits: policy.Resources{CPUMillicores: 500}, capped: make(chan struct{}), cpuLimit: 10_000_000}
	j.accounting = func() (int64, error) { return 0, errors.New("query refused") }
	j.notify = &jobNotifier{done: make(chan struct{})}
	j.notify.recordStop(errors.New("completion port closed"))
	exit, err := classifyExit(&session{}, j, &ttyExitError{code: 3})
	if exit.Reason != sandbox.SessionUnenforceable {
		t.Errorf("Reason = %v, want unenforceable", exit.Reason)
	}
	if !errdefs.IsNotAvailable(err) || errdefs.IsBudgetExceeded(err) {
		t.Errorf("err = %v, want NotAvailable and not BudgetExceeded", err)
	}
}

// With the accounting readable and the budget untouched, an exit is just an exit:
// the lost report costs attribution, not the exit code.
func TestClassifyCPUExitKeepsItsCodeWhenTheBudgetWasNotReached(t *testing.T) {
	j := &job{limits: policy.Resources{CPUMillicores: 500}, capped: make(chan struct{}), cpuLimit: 10_000_000}
	j.accounting = func() (int64, error) { return 1, nil }
	j.notify = &jobNotifier{done: make(chan struct{})}
	j.notify.recordStop(errors.New("completion port closed"))
	exit, err := classifyExit(&session{}, j, &ttyExitError{code: 3})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if exit.Reason != sandbox.SessionExited || exit.Code != 3 {
		t.Errorf("exit = %+v, want exited with code 3", exit)
	}
}

// A caller's Terminate outranks a lost verdict channel.
func TestClassifyTerminatedOutranksALostReport(t *testing.T) {
	j := &job{limits: policy.Resources{CPUMillicores: 500}, capped: make(chan struct{}), cpuLimit: 10_000_000}
	j.accounting = func() (int64, error) { return 0, errors.New("query refused") }
	j.notify = &jobNotifier{done: make(chan struct{})}
	j.notify.recordStop(errors.New("completion port closed"))
	exit, err := classifyExit(&session{terminated: true}, j, &ttyExitError{code: 1})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if exit.Reason != sandbox.SessionTerminated {
		t.Errorf("Reason = %v, want terminated", exit.Reason)
	}
}

// The sampler's verdict outranks a deadline that was about to fire.
func TestClassifySamplerFailureOutranksATimeout(t *testing.T) {
	j := &job{limits: policy.Resources{MemoryBytes: 1 << 20}, capped: make(chan struct{})}
	j.markUnenforceable(errors.New("query refused"))
	exit, err := classifyExit(&session{timedOut: true}, j, nil)
	if exit.Reason != sandbox.SessionUnenforceable {
		t.Errorf("Reason = %v, want unenforceable over a timeout", exit.Reason)
	}
	if exit.Code != -1 {
		t.Errorf("Code = %d, want -1", exit.Code)
	}
	if !errdefs.IsNotAvailable(err) {
		t.Errorf("err = %v, want NotAvailable", err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errdefs.IsBudgetExceeded(err) {
		t.Errorf("err = %v, must be neither a deadline nor a budget breach", err)
	}
}

// A cpu budget that did report keeps its budget verdict.
func TestClassifyCPUReportIsStillABudgetBreach(t *testing.T) {
	j := &job{limits: policy.Resources{CPUMillicores: 500}, capped: make(chan struct{})}
	j.markCap("cpu")
	j.notify = &jobNotifier{done: make(chan struct{})}
	exit, err := classifyExit(&session{}, j, &ttyExitError{code: 1})
	if exit.Reason != sandbox.SessionBudgetExceeded {
		t.Errorf("Reason = %v, want budget exceeded", exit.Reason)
	}
	if !errdefs.IsBudgetExceeded(err) {
		t.Errorf("err = %v, want BudgetExceeded", err)
	}
}

// With both budgets configured, the cpu report arrives from the notifier's
// goroutine while the memory sampler runs in its own. The verdicts are
// first-wins in one direction: a host is never told both "you exceeded a budget"
// and "nothing was measured".
func TestBothVerdictsFirstWins(t *testing.T) {
	t.Run("cap first", func(t *testing.T) {
		j := &job{capped: make(chan struct{})}
		j.markCap("cpu")
		j.markUnenforceable(errors.New("memory sampler broke"))
		if got := j.budgetCap(); got != "cpu" {
			t.Errorf("budgetCap = %q, want the reported cap", got)
		}
		if err := j.unenforceable(); err != nil {
			t.Errorf("unenforceable = %v, want nil after a cap was reported", err)
		}
	})
	t.Run("unenforceable first", func(t *testing.T) {
		j := &job{capped: make(chan struct{})}
		j.markUnenforceable(errors.New("memory sampler broke"))
		j.markCap("cpu")
		if got := j.budgetCap(); got != "" {
			t.Errorf("budgetCap = %q, want empty: the guarantee broke first", got)
		}
		if err := j.unenforceable(); err == nil {
			t.Error("unenforceable = nil, want the sampler failure")
		}
	})
}
