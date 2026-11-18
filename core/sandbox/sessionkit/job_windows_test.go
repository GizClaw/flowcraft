//go:build windows

package sessionkit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
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
	j, err := createJob(sandbox.ResourceLimits{MemoryBytes: 1 << 20}, time.Second)
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
	j, err := createJob(sandbox.ResourceLimits{MemoryBytes: 1 << 20}, time.Second)
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
// still claims it. The verdict is unenforceable, not a budget breach.
func TestSamplerFailureFailsClosedAsUnenforceable(t *testing.T) {
	previous := jobMemorySampleInterval
	jobMemorySampleInterval = time.Millisecond
	t.Cleanup(func() { jobMemorySampleInterval = previous })

	j, err := createJob(sandbox.ResourceLimits{MemoryBytes: 1 << 20}, time.Minute)
	if err != nil {
		t.Fatalf("createJob: %v", err)
	}
	defer func() { _ = j.close() }()
	j.sample = func() (uintptr, error) { return 0, errors.New("query refused") }

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
	if cap := j.budgetCap(); cap != "" {
		t.Errorf("budgetCap = %q, want empty: nothing was measured", cap)
	}
}

// One flaky sample must not kill an innocent session; a good one resets the count.
func TestSamplerFailureResetsOnASuccessfulSample(t *testing.T) {
	previous := jobMemorySampleInterval
	jobMemorySampleInterval = time.Millisecond
	t.Cleanup(func() { jobMemorySampleInterval = previous })

	j, err := createJob(sandbox.ResourceLimits{MemoryBytes: 1 << 20}, time.Minute)
	if err != nil {
		t.Fatalf("createJob: %v", err)
	}
	defer func() { _ = j.close() }()
	calls := 0
	j.sample = func() (uintptr, error) {
		calls++
		if calls%2 == 0 {
			return 0, nil // every second sample works
		}
		return 0, errors.New("transient")
	}

	done := make(chan struct{})
	defer close(done)
	j.enforceCaps(done)
	time.Sleep(20 * maxCapSampleFailures * jobMemorySampleInterval)
	if err := j.unenforceable(); err != nil {
		t.Fatalf("declared unenforceable on alternating samples: %v", err)
	}
}

// A released job cannot be sampled, and that must not count as a failure: the
// sampler wakes between release and the session's done channel closing.
func TestSamplerIgnoresAReleasedJob(t *testing.T) {
	j, err := createJob(sandbox.ResourceLimits{MemoryBytes: 1 << 20}, time.Minute)
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

// A dead cpu verdict channel is unenforceable, not a plain failure: the kernel
// still ends the job, but nothing can attribute the exit to the budget.
func TestClassifyCPUVerdictLossIsUnenforceable(t *testing.T) {
	j := &job{limits: sandbox.ResourceLimits{CPUMillicores: 500}, capped: make(chan struct{})}
	j.notify = &jobNotifier{done: make(chan struct{})}
	j.notify.recordStop(errors.New("completion port closed"))
	exit, err := classifyExit(&session{}, j, &ttyExitError{code: 1})
	if exit.Reason != sandbox.SessionUnenforceable {
		t.Errorf("Reason = %v, want unenforceable", exit.Reason)
	}
	if !errdefs.IsNotAvailable(err) || errdefs.IsBudgetExceeded(err) {
		t.Errorf("err = %v, want NotAvailable and not BudgetExceeded", err)
	}
}

// The sampler's verdict outranks a deadline that was about to fire.
func TestClassifySamplerFailureOutranksATimeout(t *testing.T) {
	j := &job{limits: sandbox.ResourceLimits{MemoryBytes: 1 << 20}, capped: make(chan struct{})}
	j.markUnenforceable(errors.New("query refused"))
	exit, err := classifyExit(&session{timedOut: true}, j, nil)
	if exit.Reason != sandbox.SessionUnenforceable {
		t.Errorf("Reason = %v, want unenforceable over a timeout", exit.Reason)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, must not surface as a deadline", err)
	}
}

// A cpu budget that did report keeps its budget verdict.
func TestClassifyCPUReportIsStillABudgetBreach(t *testing.T) {
	j := &job{limits: sandbox.ResourceLimits{CPUMillicores: 500}, capped: make(chan struct{})}
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
