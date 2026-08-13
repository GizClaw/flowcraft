//go:build unix

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/GizClaw/flowcraft/core/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// groupCapsAvailable reports whether group-level resource caps can be
// enforced by a sampling watcher: true when ps(1) process-group
// accounting actually works here, false otherwise — limits are then
// rejected with errdefs.NotAvailable rather than silently skipped.
func groupCapsAvailable() bool { return groupSamplingUsable() }

// groupSamplingUsable probes enforceability by running the very sample
// the watcher depends on, once per process. exec.LookPath is not
// enough: it only checks that a ps binary exists and carries the
// execute bit, which stays true inside a restricted environment
// (seccomp or MAC policy, a denied fork) where exec of ps fails at call
// time. Trusting LookPath there makes Enforcement report
// MemoryCap/CPUCap while every sample errors out and no cap ever
// fires — silent non-enforcement, the one outcome this package promises
// not to produce.
//
// The result is cached: whether ps can be executed at all is a property
// of the process environment rather than of an individual call, and
// Exec consults it on every invocation.
var groupSamplingUsable = sync.OnceValue(func() bool {
	_, _, err := sampleGroupFn(syscall.Getpgrp())
	return err == nil
})

const groupWatchInterval = 250 * time.Millisecond

// maxSampleFailures is how many consecutive sampling errors the watcher
// tolerates before declaring the caps unenforceable. One flaky ps run
// (a transient fork failure under load) must not kill an innocent
// group, but blindness must not be unbounded either: at the interval
// above, three strikes bound it to roughly 750ms.
const maxSampleFailures = 3

// sampleGroupFn indirects the sampler so tests can simulate one that
// stops working mid-run.
var sampleGroupFn = sampleGroup

// GroupCapsWatcher enforces MemoryBytes / cpu-time caps on a child
// process group by sampling aggregate usage via ps and killing the
// whole group on overflow. It exists because per-process rlimits
// cannot do the job honestly on this platform matrix: macOS rejects
// RLIMIT_AS outright, and Go children swallow SIGXCPU so RLIMIT_CPU
// never terminates them. Group-level accounting also matches the
// blast-radius intent: a child that forks N processes to split its
// memory footprint still trips the cap on the sum.
//
// The type is exported for sandbox.Runner backend authors (e.g.
// integrations/sandbox/seatbelt): start it after launching a child that leads
// its own process group, and Stop it after reaping the child.
type GroupCapsWatcher struct {
	// ctx is captured at Start time so telemetry emitted by the
	// sampler (notably a failed SIGKILL) carries the originating
	// Runner.Exec context — its trace span, deadline, and any
	// downstream caller attributes. Background if the caller passed
	// nil; we never derive from inside the sampler.
	ctx      context.Context
	pgid     int
	maxRSSKB int64
	maxCPU   time.Duration
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
	exceeded atomic.Int32

	// killReason is captured just before SIGKILL is delivered so a
	// failed kill can be reported in telemetry with the original
	// reason (memory cap, cpu cap, sample breakdown) attached.
	killReason atomic.Pointer[string]

	// sampleErr is set when sampling broke down and the watcher killed
	// the group rather than keep guarding it blindly.
	sampleErr atomic.Pointer[error]
}

const (
	groupCapNone int32 = iota
	groupCapMemory
	groupCapCPU
)

// StartGroupCapsWatcher launches sampling for pgid against the caps
// derived from res (MemoryBytes) and res x timeout (cpu-time; see
// deriveGroupCaps). It returns nil when neither cap is actionable, so
// callers may invoke Stop unconditionally. Stop must follow the
// child's Wait; stopping is synchronous so no ps invocation can
// outlive the Exec call.
func StartGroupCapsWatcher(ctx context.Context, pgid int, res ResourceLimits, timeout time.Duration) *GroupCapsWatcher {
	maxRSSKB, maxCPU := deriveGroupCaps(res, timeout)
	if maxRSSKB == 0 && maxCPU == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w := &GroupCapsWatcher{
		ctx:      ctx,
		pgid:     pgid,
		maxRSSKB: maxRSSKB,
		maxCPU:   maxCPU,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	go w.run()
	return w
}

func (w *GroupCapsWatcher) run() {
	t := time.NewTicker(groupWatchInterval)
	defer t.Stop()
	defer close(w.doneCh)
	failures := 0
	for {
		select {
		case <-w.stopCh:
			return
		case <-t.C:
			rssKB, cpu, err := sampleGroupFn(w.pgid)
			if err != nil {
				// A flaky ps run must not kill an innocent group, but a
				// sampler that stays broken means the caps the caller
				// asked for have stopped being enforced. Polling on
				// forever would let the child run unbounded while the
				// watcher pretends to guard it, so fail closed once the
				// failure looks persistent.
				failures++
				if failures < maxSampleFailures {
					continue
				}
				wrapped := fmt.Errorf("group sampling failed %d times in a row: %w", failures, err)
				w.sampleErr.Store(&wrapped)
				reason := "sample_failure"
				w.killReason.Store(&reason)
				w.killGroup()
				return
			}
			failures = 0
			if w.maxRSSKB > 0 && rssKB >= w.maxRSSKB {
				w.exceeded.Store(groupCapMemory)
				reason := "memory_cap"
				w.killReason.Store(&reason)
				w.killGroup()
				return
			}
			if w.maxCPU > 0 && cpu >= w.maxCPU {
				w.exceeded.Store(groupCapCPU)
				reason := "cpu_cap"
				w.killReason.Store(&reason)
				w.killGroup()
				return
			}
		}
	}
}

func (w *GroupCapsWatcher) killGroup() {
	// The child leads its own group (Setpgid), so pid == pgid.
	//
	// A failed SIGKILL is rare but consequential: the cap we tripped
	// then stops being enforced. Surface it as a warning so an operator
	// can see the child was left running unconstrained.
	if err := syscall.Kill(-w.pgid, syscall.SIGKILL); err != nil {
		attrs := []otellog.KeyValue{
			otellog.String("sandbox.pgid", strconv.Itoa(w.pgid)),
		}
		if rp := w.killReason.Load(); rp != nil {
			attrs = append(attrs, otellog.String("sandbox.kill_reason", *rp))
		}
		telemetry.WarnErr(w.ctx,
			"sandbox: failed to deliver SIGKILL to process group",
			err, attrs...)
	}
}

// Stop ends sampling and waits for the sampler goroutine to exit. It
// is nil-safe so callers can defer it without checking whether
// StartGroupCapsWatcher returned a watcher at all.
func (w *GroupCapsWatcher) Stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() { close(w.stopCh) })
	<-w.doneCh
}

// Unenforceable returns a non-nil error when the watcher gave up on
// sampling and killed the group because the requested caps could no
// longer be measured. It is mutually exclusive with Exceeded — the
// sampler stops at whichever condition it reaches first — and callers
// should consult it first, surfacing errdefs.NotAvailable: nothing was
// shown to exceed a budget, the budget stopped being observable.
func (w *GroupCapsWatcher) Unenforceable() error {
	if w == nil {
		return nil
	}
	if p := w.sampleErr.Load(); p != nil {
		return *p
	}
	return nil
}

// Exceeded reports which configured cap terminated the process group.
// The empty string means the watcher did not trigger (the process may
// have exited on its own, been cancelled by its context, or been killed
// because sampling broke down — see Unenforceable).
func (w *GroupCapsWatcher) Exceeded() string {
	if w == nil {
		return ""
	}
	switch w.exceeded.Load() {
	case groupCapMemory:
		return "memory"
	case groupCapCPU:
		return "cpu"
	default:
		return ""
	}
}

// sampleGroup sums RSS (KiB) and cpu-time across every live member of
// the process group. A group with no surviving members reports zeros,
// which never trips a cap.
func sampleGroup(pgid int) (rssKB int64, cpu time.Duration, err error) {
	out, err := exec.Command("ps", "-o", "pgid=,rss=,time=", "-ax").Output()
	if err != nil {
		return 0, 0, fmt.Errorf("ps: %w", err)
	}
	target := strconv.Itoa(pgid)
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != target {
			continue
		}
		rss, perr := strconv.ParseInt(fields[1], 10, 64)
		if perr != nil {
			continue
		}
		rssKB += rss
		cpu += parseProcCPUTime(fields[2])
	}
	return rssKB, cpu, nil
}

// parseProcCPUTime parses ps TIME columns in the shapes "mm:ss",
// "mm:ss.ff" (macOS), "hh:mm:ss", and "dd-hh:mm:ss" (Linux); fractional
// seconds are truncated. Unparseable input yields zero.
func parseProcCPUTime(s string) time.Duration {
	var days int64
	if i := strings.IndexByte(s, '-'); i >= 0 {
		days, _ = strconv.ParseInt(s[:i], 10, 64)
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0
	}
	if i := strings.IndexByte(parts[len(parts)-1], '.'); i >= 0 {
		parts[len(parts)-1] = parts[len(parts)-1][:i]
	}
	nums := make([]int64, len(parts))
	for i, p := range parts {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return 0
		}
		nums[i] = n
	}
	secs := nums[len(nums)-1]
	mins := nums[len(nums)-2]
	var hours int64
	if len(nums) == 3 {
		hours = nums[0]
	}
	return time.Duration(days*24*3600+hours*3600+mins*60+secs) * time.Second
}
