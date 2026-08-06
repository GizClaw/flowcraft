//go:build unix

package sandbox

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestGroupCapsAvailableAgreesWithSampler pins the probe to the thing it
// claims to predict. The bug this replaces used exec.LookPath, which
// reports success for a ps binary that exists and is marked executable
// even where exec of it is denied — so Enforcement advertised
// MemoryCap/CPUCap while every sample failed and no cap ever fired.
func TestGroupCapsAvailableAgreesWithSampler(t *testing.T) {
	_, _, err := sampleGroup(syscall.Getpgrp())
	if want := err == nil; groupCapsAvailable() != want {
		t.Fatalf("groupCapsAvailable() = %v, but sampling %s (err=%v)",
			groupCapsAvailable(), map[bool]string{true: "works", false: "fails"}[want], err)
	}
}

// TestWatcherFailsClosedWhenSamplingBreaks covers a sampler that dies
// mid-run. Tolerating that forever would leave the child unbounded while
// the watcher pretended to guard it, so the group is killed and the
// failure surfaces as Unenforceable — never as Exceeded, since no budget
// was observed to be exceeded.
func TestWatcherFailsClosedWhenSamplingBreaks(t *testing.T) {
	sentinel := errors.New("sampler is down")
	restore := sampleGroupFn
	sampleGroupFn = func(int) (int64, time.Duration, error) { return 0, 0, sentinel }
	t.Cleanup(func() { sampleGroupFn = restore })

	c := exec.Command("sleep", "30")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Skipf("cannot spawn a child here: %v", err)
	}

	w := StartGroupCapsWatcher(context.Background(), c.Process.Pid, ResourceLimits{MemoryBytes: 128 << 20}, time.Second)
	if w == nil {
		t.Fatal("an actionable MemoryBytes cap must produce a watcher")
	}

	// The child must die from the watcher, not from the test timing out.
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		t.Fatal("watcher never killed the group despite a permanently broken sampler")
	}
	w.Stop()

	err := w.Unenforceable()
	if err == nil {
		t.Fatal("Unenforceable() = nil; a sampler that never recovers means the cap is not enforced")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("Unenforceable() = %v, want it to wrap the sampler error", err)
	}
	if got := w.Exceeded(); got != "" {
		t.Errorf("Exceeded() = %q; nothing was measured, so no cap can be reported as exceeded", got)
	}
}

// TestWatcherToleratesTransientSampleFailure keeps the fail-closed path
// from becoming trigger-happy: a single hiccup under load must not kill
// a healthy group.
func TestWatcherToleratesTransientSampleFailure(t *testing.T) {
	calls := 0
	restore := sampleGroupFn
	sampleGroupFn = func(int) (int64, time.Duration, error) {
		calls++
		if calls <= maxSampleFailures-1 {
			return 0, 0, errors.New("transient")
		}
		return 0, 0, nil // healthy: well under the cap
	}
	t.Cleanup(func() { sampleGroupFn = restore })

	c := exec.Command("sleep", "30")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Skipf("cannot spawn a child here: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		_ = c.Wait()
	})

	w := StartGroupCapsWatcher(context.Background(), c.Process.Pid, ResourceLimits{MemoryBytes: 128 << 20}, time.Second)
	time.Sleep(groupWatchInterval * time.Duration(maxSampleFailures+2))
	w.Stop()

	if err := w.Unenforceable(); err != nil {
		t.Errorf("Unenforceable() = %v; failures below the threshold must be forgiven", err)
	}
	if got := w.Exceeded(); got != "" {
		t.Errorf("Exceeded() = %q, want no cap trip", got)
	}
	if c.ProcessState != nil {
		t.Error("healthy group was killed")
	}
}
