//go:build unix

package sandbox

import (
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// groupCapsAvailable reports whether group-level resource caps can be
// enforced by a sampling watcher. True on unix (ps with pgid/rss/time
// is available), false elsewhere — limits are then rejected with
// errdefs.NotAvailable rather than silently skipped.
func groupCapsAvailable() bool {
	_, err := exec.LookPath("ps")
	return err == nil
}

const groupWatchInterval = 250 * time.Millisecond

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
// sdkx/sandbox/seatbelt): start it after launching a child that leads
// its own process group, and Stop it after reaping the child.
type GroupCapsWatcher struct {
	pgid     int
	maxRSSKB int64
	maxCPU   time.Duration
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
	exceeded atomic.Int32
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
func StartGroupCapsWatcher(pgid int, res ResourceLimits, timeout time.Duration) *GroupCapsWatcher {
	maxRSSKB, maxCPU := deriveGroupCaps(res, timeout)
	if maxRSSKB == 0 && maxCPU == 0 {
		return nil
	}
	w := &GroupCapsWatcher{
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
	for {
		select {
		case <-w.stopCh:
			return
		case <-t.C:
			rssKB, cpu, err := sampleGroup(w.pgid)
			if err != nil {
				continue // a flaky ps run must not kill an innocent group
			}
			if w.maxRSSKB > 0 && rssKB >= w.maxRSSKB {
				w.exceeded.Store(groupCapMemory)
				w.killGroup()
				return
			}
			if w.maxCPU > 0 && cpu >= w.maxCPU {
				w.exceeded.Store(groupCapCPU)
				w.killGroup()
				return
			}
		}
	}
}

func (w *GroupCapsWatcher) killGroup() {
	// The child leads its own group (Setpgid), so pid == pgid.
	_ = syscall.Kill(-w.pgid, syscall.SIGKILL)
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

// Exceeded reports which configured cap terminated the process group.
// The empty string means the watcher did not trigger (the process may
// have exited itself or been cancelled by its context).
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
		return 0, 0, err
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
