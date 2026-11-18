//go:build windows

package sessionkit

import (
	"testing"
	"time"

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
