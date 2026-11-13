//go:build unix && !aix

package sessionkit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel/attribute"
)

// groupCapsAvailable reports whether group-level resource caps can be
// enforced by a sampling watcher: true when ps(1) process-group
// accounting actually works here, false otherwise — limits are then
// rejected with errdefs.NotAvailable rather than silently skipped.
func groupCapsAvailable() bool { return groupSamplingUsable() }

// groupSamplingUsable probes enforceability by running the very sample
// the watcher depends on, once per process. exec.LookPath is not
// enough on platforms that fork ps: it only checks that a ps binary
// exists and carries the execute bit, which stays true inside a
// restricted environment (seccomp or MAC policy, a denied fork) where
// exec of ps fails at call time. Trusting LookPath there makes
// Enforcement report MemoryCap/CPUCap while every sample errors out and
// no cap ever fires — silent non-enforcement, the one outcome this
// package promises not to produce.
//
// The result is cached: whether ps can be executed at all is a property
// of the process environment rather than of an individual call, and
// Exec consults it on every invocation.
var groupSamplingUsable = sync.OnceValue(func() bool {
	_, err := sampleHostFn()
	return err == nil
})

// groupWatchInterval is the sampling cadence. It is a variable so a test can
// silence the automatic ticker and drive the sampler deterministically.
var groupWatchInterval = 250 * time.Millisecond

// maxSampleFailures is how many consecutive sampling errors the watcher
// tolerates before declaring the caps unenforceable. One flaky ps run
// (a transient fork failure under load) must not kill an innocent
// group, but blindness must not be unbounded either: at the interval
// above, three strikes bound it to roughly 750ms.
const maxSampleFailures = 3

// sampleGroupFn indirects the sampler so tests can simulate one that
// stops working mid-run.
var sampleHostFn = sampleHost

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
// core/sandbox-seatbelt): start it after launching a child that leads
// its own process group, and Stop it after reaping the child.
// Stop must follow the child's Wait; stopping is synchronous so no
// sampler invocation can outlive the Exec call.
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
	doneCh   chan struct{}
	doneOnce sync.Once
	exceeded atomic.Int32

	// failures counts consecutive sampling errors. The host sampler owns the
	// lock that guards it (observe runs under it), so no atomic is needed.
	failures int

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
// callers may invoke Stop unconditionally.
func StartGroupCapsWatcher(ctx context.Context, pgid int, res sandbox.ResourceLimits, timeout time.Duration) *GroupCapsWatcher {
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
		doneCh:   make(chan struct{}),
	}
	// One sampler goroutine per process, not one per session: sampling the host
	// is the same work whoever asked, and it is the expensive half.
	sharedSampler.add(w)
	return w
}

// observe applies one host sample to this watcher and reports whether it tripped
// (and should stop watching). It only touches this watcher's own state: the
// sampler calls it while holding its lock, so it must not call back into it.
func (w *GroupCapsWatcher) observe(usage groupUsage, sampleErr error) bool {
	if sampleErr != nil {
		// A flaky sample must not kill an innocent group, but a sampler that
		// stays broken means the caps the caller asked for have stopped being
		// enforced. Polling on forever would let the child run unbounded while
		// the watcher pretends to guard it, so fail closed once the failure
		// looks persistent.
		w.failures++
		if w.failures < maxSampleFailures {
			return false
		}
		wrapped := fmt.Errorf("group sampling failed %d times in a row: %w", w.failures, sampleErr)
		w.sampleErr.Store(&wrapped)
		reason := "sample_failure"
		w.killReason.Store(&reason)
		return true
	}
	w.failures = 0
	if w.maxRSSKB > 0 && usage.rssKB >= w.maxRSSKB {
		w.exceeded.Store(groupCapMemory)
		reason := "memory_cap"
		w.killReason.Store(&reason)
		return true
	}
	if w.maxCPU > 0 && usage.cpu >= w.maxCPU {
		w.exceeded.Store(groupCapCPU)
		reason := "cpu_cap"
		w.killReason.Store(&reason)
		return true
	}
	return false
}

// finish closes the watcher's done channel exactly once.
func (w *GroupCapsWatcher) finish() { w.doneOnce.Do(func() { close(w.doneCh) }) }

func (w *GroupCapsWatcher) killGroup() {
	// The child leads its own group (Setpgid), so pid == pgid.
	//
	// The kill itself is the enforcement event: a memory/CPU cap trip or
	// a sampling failure silently stopping the group is exactly what an
	// operator needs to see in logs, so record it before delivery.
	attrs := []attribute.KeyValue{
		attribute.String("sandbox.pgid", strconv.Itoa(w.pgid)),
	}
	if rp := w.killReason.Load(); rp != nil {
		attrs = append(attrs, attribute.String("sandbox.kill_reason", *rp))
	}
	if sp := w.sampleErr.Load(); sp != nil {
		attrs = append(attrs, attribute.String(telemetry.AttrErrorMessage, (*sp).Error()))
	}
	telemetry.Warn(w.ctx, "sandbox: process group killed by resource watcher", attrs...)

	// A failed SIGKILL is rare but consequential: the cap we tripped
	// then stops being enforced. Surface it as a warning so an operator
	// can see the child was left running unconstrained.
	if err := killGroupFn(w.pgid); err != nil {
		killAttrs := []attribute.KeyValue{
			attribute.String("sandbox.pgid", strconv.Itoa(w.pgid)),
		}
		if rp := w.killReason.Load(); rp != nil {
			killAttrs = append(killAttrs, attribute.String("sandbox.kill_reason", *rp))
		}
		telemetry.WarnErr(w.ctx,
			"sandbox: failed to deliver SIGKILL to process group",
			err, killAttrs...)
	}
}

// Stop ends sampling and waits for the sampler goroutine to exit. It
// is nil-safe so callers can defer it without checking whether
// StartGroupCapsWatcher returned a watcher at all.
func (w *GroupCapsWatcher) Stop() {
	if w == nil {
		return
	}
	sharedSampler.remove(w)
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

// groupUsage is one process group's sampled footprint.
type groupUsage struct {
	rssKB int64
	cpu   time.Duration
}

// sampleHost aggregates every process group on this host in one pass. On Linux it
// reads /proc; elsewhere it runs one ps. It is a variable so the sampler logic can
// be tested without a process table.

// hostSampler shares one sampling pass per tick between every watcher in this
// process.
//
// Sampling the host is the expensive half of enforcement — a full ps invocation
// (tens of milliseconds on a busy machine) or a /proc walk over every process —
// and the answer is the same for every session on that host. Running it per
// session made the cost grow with the number of sessions for no extra
// information.
type hostSampler struct {
	mu      sync.Mutex
	entries map[*GroupCapsWatcher]struct{}
	running bool
	stop    chan struct{}
	done    chan struct{}
}

var sharedSampler = &hostSampler{entries: make(map[*GroupCapsWatcher]struct{})}

func (s *hostSampler) add(w *GroupCapsWatcher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[w] = struct{}{}
	if s.running {
		return
	}
	s.running = true
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go s.run(s.stop, s.done)
}

func (s *hostSampler) remove(w *GroupCapsWatcher) {
	s.mu.Lock()
	delete(s.entries, w)
	var done chan struct{}
	if len(s.entries) == 0 && s.running {
		s.running = false
		close(s.stop)
		done = s.done
	}
	s.mu.Unlock()

	w.finish()
	if done != nil {
		// The sampler exited, so no observe can be in flight for a watcher that
		// just unregistered.
		<-done
	}
}

func (s *hostSampler) run(stop, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(groupWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

// tick runs one sampling pass and applies it to every watcher.
func (s *hostSampler) tick() {
	usage, sampleErr := sampleHostFn()

	s.mu.Lock()
	var tripped []*GroupCapsWatcher
	for w := range s.entries {
		if w.observe(usage[w.pgid], sampleErr) {
			tripped = append(tripped, w)
		}
	}
	s.mu.Unlock()

	// The kill is a syscall and a telemetry record; keep it out of the lock so a
	// slow logger cannot delay the next sample.
	for _, w := range tripped {
		w.killGroup()
		s.remove(w)
	}
}

// killGroupFn delivers the terminating signal to a process group. It is a
// variable so a test can assert the enforcement decision without signalling a
// real group.
var killGroupFn = func(pgid int) error { return syscall.Kill(-pgid, syscall.SIGKILL) }

// sampleHost aggregates every process group on the host in one pass.
func sampleHost() (map[int]groupUsage, error) {
	if runtime.GOOS == "linux" {
		return sampleHostLinux()
	}
	return sampleHostPS()
}

// sampleHostPS is the portable fallback: one ps invocation that scans the system
// process table, grouped here by pgid.
func sampleHostPS() (map[int]groupUsage, error) {
	out, err := exec.Command("ps", "-o", "pgid=,rss=,time=", "-ax").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	usage := make(map[int]groupUsage)
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		pgid, perr := strconv.Atoi(fields[0])
		if perr != nil {
			continue
		}
		rss, perr := strconv.ParseInt(fields[1], 10, 64)
		if perr != nil {
			continue
		}
		u := usage[pgid]
		u.rssKB += rss
		u.cpu += parseProcCPUTime(fields[2])
		usage[pgid] = u
	}
	return usage, nil
}

// sampleHostLinux reads /proc once and groups the result by process group.
func sampleHostLinux() (map[int]groupUsage, error) {
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("proc: %w", err)
	}
	pageSize := int64(os.Getpagesize())
	usage := make(map[int]groupUsage)
	for _, proc := range procs {
		if !proc.IsDir() {
			continue
		}
		if _, perr := strconv.Atoi(proc.Name()); perr != nil {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join("/proc", proc.Name(), "stat"))
		if rerr != nil {
			continue // process exited between readdir and stat
		}
		fields, ok := parseProcStat(data)
		if !ok {
			continue
		}
		u := usage[fields.pgrp]
		u.rssKB += fields.rssPages * pageSize / 1024
		u.cpu += time.Duration(fields.utimeTicks+fields.stimeTicks) * 10 * time.Millisecond
		usage[fields.pgrp] = u
	}
	return usage, nil
}

type procStatFields struct {
	pgrp       int
	utimeTicks int64
	stimeTicks int64
	rssPages   int64
}

// parseProcStat parses the numeric fields of /proc/<pid>/stat. The
// comm field may contain spaces and parentheses, so parsing starts
// after the final ')' and field indices are relative to that point
// (0-based: state=0, ppid=1, pgrp=2, utime=11, stime=12, rss=21).
func parseProcStat(data []byte) (procStatFields, bool) {
	lastParen := -1
	for i, b := range data {
		if b == ')' {
			lastParen = i
		}
	}
	if lastParen < 0 || lastParen+1 >= len(data) {
		return procStatFields{}, false
	}
	fields := strings.Fields(string(data[lastParen+1:]))
	if len(fields) < 22 {
		return procStatFields{}, false
	}
	pgrp, err1 := strconv.Atoi(fields[2])
	utime, err2 := strconv.ParseInt(fields[11], 10, 64)
	stime, err3 := strconv.ParseInt(fields[12], 10, 64)
	rss, err4 := strconv.ParseInt(fields[21], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return procStatFields{}, false
	}
	return procStatFields{
		pgrp:       pgrp,
		utimeTicks: utime,
		stimeTicks: stime,
		rssPages:   rss,
	}, true
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
