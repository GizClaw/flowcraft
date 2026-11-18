//go:build windows

package sessionkit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"go.opentelemetry.io/otel/attribute"
	xwin "golang.org/x/sys/windows"
)

// job wraps a Windows Job Object. A job is the kernel-level process
// group: processes assigned to it are managed as a unit, so
// terminating the job kills every descendant (not just the leader)
// and job limits apply across the whole tree.
//
// The job owns the cap verdict as well as the handle: both the kernel's
// completion-port report and the memory sampler record it here, and a cap is
// always recorded before anything is killed, so a classification that runs while
// the kill is in flight cannot miss it.
type job struct {
	h      xwin.Handle
	notify *jobNotifier           // nil unless a cpu budget was configured
	limits sandbox.ResourceLimits // what the caller asked for

	mu     sync.Mutex
	closed bool // the handle has been (or is being) released
	// verdict is one of "memory" or "cpu": the cap the kernel or the sampler
	// reported as exceeded. verdictErr is why the caps stopped being measurable.
	// The two are mutually exclusive in one direction — whichever goroutine
	// reports first wins — because a host must not be told both "you exceeded a
	// budget" and "nothing was measured", and the first verdict is the one that
	// explains why the job ended.
	verdict    string
	verdictErr error
	cpuLimit   int64 // the job-time budget in 100ns units, 0 when unset
	failures   int   // consecutive failed samples, reset by a good one
	capped     chan struct{}
	once       sync.Once
	// sample and accounting are fields for the same reason: a test can simulate a
	// watcher that stops working mid-run, the way the unix watcher's sampleHostFn
	// does.
	sample     func() (uintptr, error)
	accounting func() (int64, error)
}

// Job completion-port message codes (winnt.h). The numbers are not adjacent —
// 5 is unused and the two memory codes come after the process ones — so they are
// spelled out rather than inferred.
const (
	jobObjectMsgEndOfJobTime     = 1  // JOB_OBJECT_MSG_END_OF_JOB_TIME
	jobObjectMsgEndOfProcessTime = 2  // JOB_OBJECT_MSG_END_OF_PROCESS_TIME
	jobObjectMsgJobMemoryLimit   = 10 // JOB_OBJECT_MSG_JOB_MEMORY_LIMIT

	// notifierKey is the completion key recorded in the job
	// association; notifierStop is the sentinel posted to shut the
	// notifier down. The two must differ so a stop cannot be confused
	// with a job message (which carries notifierKey).
	notifierKey  = uintptr(0x5A5A5A5A)
	notifierStop = uintptr(0x5A5A5A5B)
)

// associateCompletionPort mirrors JOBOBJECT_ASSOCIATE_COMPLETION_PORT.
type associateCompletionPort struct {
	CompletionKey  uintptr
	CompletionPort xwin.Handle
}

// jobNotifier drains a job object's completion port and reports which cap the
// kernel enforced, so a cpu-budget kill can be classified as
// SessionBudgetExceeded instead of a plain exit. Memory travels the sampler's
// path instead — see enforceCaps — but a memory report is still honoured if the
// kernel ever produces one.
type jobNotifier struct {
	port  xwin.Handle
	done  chan struct{}
	onCap func(string)

	mu       sync.Mutex
	err      error // the drain died; the verdicts it carried are lost
	stopping bool  // stop() was called: an aborted wait is expected
}

// run drains completion messages until stop posts the sentinel or the port is
// closed.
func (n *jobNotifier) run() {
	defer close(n.done)
	for {
		var qty uint32
		var key uintptr
		var ovl *xwin.Overlapped
		if err := xwin.GetQueuedCompletionStatus(n.port, &qty, &key, &ovl, xwin.INFINITE); err != nil {
			n.recordStop(err)
			return
		}
		if key == notifierStop {
			return
		}
		if cap := capForMessage(qty); cap != "" {
			n.onCap(cap)
		}
	}
}

// recordStop remembers why the drain ended when it was not asked to. A port that
// dies mid-session takes the cpu verdict with it: the kernel still enforces the
// budget, but the exit can no longer be attributed to it.
func (n *jobNotifier) recordStop(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopping {
		return
	}
	if n.err == nil {
		n.err = err
	}
}

// failure reports why the drain ended, if it ended unasked.
func (n *jobNotifier) failure() error {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.err
}

// capForMessage maps a job completion message to the cap it reports, or "" when
// the message is not one this package acts on.
func capForMessage(qty uint32) string {
	switch qty {
	case jobObjectMsgJobMemoryLimit:
		return "memory"
	case jobObjectMsgEndOfJobTime, jobObjectMsgEndOfProcessTime:
		return "cpu"
	}
	return ""
}

// stop posts the sentinel, waits for the drain goroutine (bounded),
// and closes the port.
func (n *jobNotifier) stop() {
	if n == nil {
		return
	}
	n.mu.Lock()
	n.stopping = true
	n.mu.Unlock()
	_ = xwin.PostQueuedCompletionStatus(n.port, 0, notifierStop, nil)
	select {
	case <-n.done:
	case <-time.After(2 * time.Second):
	}
	_ = xwin.CloseHandle(n.port)
}

// newJobNotifier creates a completion port, associates it with the job
// so limit-violation messages are delivered, and starts the drain
// goroutine.
func newJobNotifier(jobHandle xwin.Handle, onCap func(string)) (*jobNotifier, error) {
	port, err := xwin.CreateIoCompletionPort(xwin.InvalidHandle, 0, 0, 1)
	if err != nil {
		return nil, errdefs.Internal(fmt.Errorf("windows: create job completion port: %w", err))
	}
	n := &jobNotifier{
		port:  port,
		done:  make(chan struct{}),
		onCap: onCap,
	}
	assoc := associateCompletionPort{CompletionKey: notifierKey, CompletionPort: port}
	if _, err := xwin.SetInformationJobObject(jobHandle,
		xwin.JobObjectAssociateCompletionPortInformation,
		uintptr(unsafe.Pointer(&assoc)), uint32(unsafe.Sizeof(assoc))); err != nil {
		_ = xwin.CloseHandle(port)
		return nil, errdefs.Internal(fmt.Errorf("windows: associate job completion port: %w", err))
	}
	go n.run()
	return n, nil
}

// createJob creates an anonymous job with limits derived from the
// policy. KILL_ON_JOB_CLOSE is always set: when the last job handle
// closes (including on spawn failure paths), every associated process
// is terminated, so a partially-started session can never leak a
// process.
func createJob(limits sandbox.ResourceLimits, timeout time.Duration) (*job, error) {
	h, err := xwin.CreateJobObject(nil, nil)
	if err != nil {
		return nil, errdefs.Internal(fmt.Errorf("windows: create job object: %w", err))
	}
	j := &job{h: h, limits: limits, capped: make(chan struct{})}
	j.sample = j.peakMemory
	j.accounting = j.userTime
	if limits.CPUMillicores > 0 {
		j.cpuLimit = cpuBudget100ns(limits, timeout)
	}
	info := jobLimits(limits, timeout)
	if _, err := xwin.SetInformationJobObject(
		h, xwin.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = j.close()
		return nil, errdefs.Internal(fmt.Errorf("windows: set job limits: %w", err))
	}
	// Only a cpu budget is a kernel limit that ends the job, so only it needs the
	// completion port. Memory is sampled, and the sampler records the cap itself.
	if limits.CPUMillicores > 0 {
		notify, err := newJobNotifier(h, j.markCap)
		if err != nil {
			_ = j.close()
			return nil, err
		}
		j.notify = notify
	}
	return j, nil
}

// markCap records the first cap that fired and releases whoever waits on it. It
// is always called before the kill, so a classification racing the kill still
// sees the cap.
func (j *job) markCap(cap string) {
	j.mu.Lock()
	won := j.verdict == "" && j.verdictErr == nil
	if won {
		j.verdict = cap
	}
	j.mu.Unlock()
	if won {
		j.once.Do(func() { close(j.capped) })
	}
}

// markUnenforceable records that the caps stopped being measurable, unless a cap
// was already reported. It is not a budget verdict: nothing was measured.
func (j *job) markUnenforceable(err error) {
	j.mu.Lock()
	if j.verdict == "" && j.verdictErr == nil {
		j.verdictErr = err
	}
	j.mu.Unlock()
}

// unenforceable reports why the caps stopped being measurable, if they did.
func (j *job) unenforceable() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.verdictErr
}

// notifierFailure reports a cpu verdict channel that died mid-session.
func (j *job) notifierFailure() error { return j.notify.failure() }

// budgetCap reports which resource cap was enforced, if any.
func (j *job) budgetCap() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.verdict
}

// enforceCaps ends the job when a cap is exceeded, and is what makes "exceeded"
// an event instead of something a later classification reconstructs. It is the
// Windows form of what the unix watcher does by sampling its process group.
//
// The cpu budget needs no help: JOB_OBJECT_LIMIT_JOB_TIME makes the kernel
// terminate the tree, and the report it posts is what markCap records.
//
// Memory is the one that does, and deliberately not through a kernel limit.
// JOB_OBJECT_LIMIT_JOB_MEMORY refuses the allocation that would go over, so the
// job's accounting never crosses the budget and the kernel has no breach to
// report: the child is left running with its allocation refused, and a child
// that retries instead of exiting (PowerShell thrashes in its allocator) reaches
// the wall-clock Timeout. Sampling the job's own peak is the only reading of
// "the budget was exceeded" that does not depend on the child's behaviour. The
// cost is the same one the unix watcher pays: a session may run past its budget
// until the next sample.
func (j *job) enforceCaps(done <-chan struct{}) {
	if j.notify == nil && j.limits.MemoryBytes <= 0 {
		return
	}
	var ticks <-chan time.Time
	var ticker *time.Ticker
	if j.limits.MemoryBytes > 0 {
		ticker = time.NewTicker(jobMemorySampleInterval)
		ticks = ticker.C
	}
	go func() {
		if ticker != nil {
			defer ticker.Stop()
		}
		for {
			select {
			case <-j.capped:
				terminateJob(j)
				return
			case <-ticks:
				over, err := j.sampleCaps()
				if err != nil {
					// A flaky sample must not kill an innocent session, but a sampler
					// that stays broken means the budget the caller asked for has stopped
					// being enforced. Polling on forever would run the child unguarded
					// while the capability still claims the cap, so fail closed once the
					// failure looks persistent — the unix watcher's rule.
					if !j.failuresPassed() {
						continue
					}
					wrapped := fmt.Errorf(
						"windows: job memory sampling failed %d times in a row: %w", j.failureCount(), err)
					j.markUnenforceable(wrapped)
					// Kill first: this path exists to bound an unguarded window, and
					// an exporter is not allowed to widen it.
					terminateJobFn(j)
					telemetry.Warn(context.Background(),
						"windows: resource caps became unenforceable, killing the job",
						attribute.String("sandbox.kill_reason", "sample_failure"),
						attribute.String("sandbox.cap", "memory"),
						attribute.String(telemetry.AttrErrorMessage, err.Error()))
					return
				}
				if over {
					j.markCap("memory")
					terminateJob(j)
					return
				}
			case <-done:
				return
			}
		}
	}()
}

// jobMemorySampleInterval is how often a configured memory budget is sampled.
// It is tighter than the unix watcher's 250ms because a job handle answers the
// same question with one kernel call instead of walking a process tree: the
// interval is what bounds how far past its budget a session can run before it is
// killed, and how long the caps can stay unobservable before the session is
// ended. It is a variable so a test can drive the sampler deterministically.
var jobMemorySampleInterval = 25 * time.Millisecond

// sampleCaps reads one sample and reports whether the budget is exceeded. An
// error is a failed sample, not a breach: the caller decides when repeated
// failures mean the caps are unenforceable.
func (j *job) sampleCaps() (bool, error) {
	// The sampler can tick between release (which marks the handle released) and
	// the session's done channel closing. Querying through the released handle
	// would log a spurious failure on every teardown.
	j.mu.Lock()
	closed := j.closed
	j.mu.Unlock()
	if closed {
		return false, nil
	}
	peak, err := j.sample()
	j.mu.Lock()
	defer j.mu.Unlock()
	if err != nil {
		j.failures++
		return false, err
	}
	j.failures = 0
	return peak > uintptr(j.limits.MemoryBytes), nil
}

// failuresPassed reports whether the failures sampleCaps has counted have reached
// the threshold for declaring the caps unenforceable.
func (j *job) failuresPassed() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.failures >= maxCapSampleFailures
}

// failureCount is how many samples have failed in a row, for the message that
// explains why the session was killed.
func (j *job) failureCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.failures
}

// basicAccounting mirrors JOBOBJECT_BASIC_ACCOUNTING_INFORMATION. x/sys has the
// information class but not the struct, and TotalUserTime is the only field this
// package asks for: it is how a cpu budget breach is attributed without the
// completion-port report.
type basicAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// cpuBreach reports whether the job's user time reached its budget.
//
// It answers the question the completion port answers, from state instead of
// from a notification: the kernel terminates a job that exhausts
// JOB_OBJECT_LIMIT_JOB_TIME, and the accounting survives that, so an exit can be
// attributed even when the report was missed. An error means the exit cannot be
// attributed at all — both the accounting and the report are gone — which is the
// one case that is a broken guarantee rather than a budget verdict.
func (j *job) cpuBreach() (bool, error) {
	if j.cpuLimit <= 0 {
		return false, nil
	}
	used, err := j.accounting()
	if err != nil {
		if notifyErr := j.notifierFailure(); notifyErr != nil {
			return false, fmt.Errorf("job accounting unreadable (%v) and the verdict channel lost: %w", err, notifyErr)
		}
		return false, nil
	}
	return used >= j.cpuLimit, nil
}

// userTime reads the job's total user time in 100ns units.
func (j *job) userTime() (int64, error) {
	var info basicAccounting
	if err := xwin.QueryInformationJobObject(j.h,
		xwin.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return 0, err
	}
	return info.TotalUserTime, nil
}

// peakMemory reports the job's peak committed memory. A timed-out session is
// diagnosed with it: an exceeded budget the kernel never acted on looks exactly
// like a slow child from the outside.
func (j *job) peakMemory() (uintptr, error) {
	var info xwin.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := xwin.QueryInformationJobObject(j.h,
		xwin.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return 0, err
	}
	return info.PeakJobMemoryUsed, nil
}

// jobLimits maps sandbox.ResourceLimits onto the Windows extended
// limit structure. MemoryBytes becomes a job-wide cap (the same
// "aggregate process group" semantics as the unix watcher);
// CPUMillicores becomes a job-wide user-time budget derived from
// Timeout x millicores/1000. When the job-wide budget is exhausted the
// kernel terminates every process in the job and posts
// JOB_OBJECT_MSG_END_OF_JOB_TIME.
func jobLimits(limits sandbox.ResourceLimits, timeout time.Duration) xwin.JOBOBJECT_EXTENDED_LIMIT_INFORMATION {
	var info xwin.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags |= xwin.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	// Memory is deliberately not a kernel limit. JOB_OBJECT_LIMIT_JOB_MEMORY
	// refuses the allocation that would exceed it, so the job's accounting never
	// crosses the budget: there is nothing for the kernel to report, the process
	// is not ended, and a child that retries its allocation runs to the wall-clock
	// Timeout. The budget is sampled instead — see enforceCaps.
	if limits.CPUMillicores > 0 {
		info.BasicLimitInformation.LimitFlags |= xwin.JOB_OBJECT_LIMIT_JOB_TIME
		info.BasicLimitInformation.PerJobUserTimeLimit = cpuBudget100ns(limits, timeout)
	}
	return info
}

// cpuBudget100ns derives the per-process user-time budget in
// 100-nanosecond units (the unit Windows job limits use) from
// CPUMillicores and the wall-clock Timeout: budget = Timeout x
// millicores / 1000, matching sandbox.ResourceLimits semantics.
func cpuBudget100ns(limits sandbox.ResourceLimits, timeout time.Duration) int64 {
	if limits.CPUMillicores <= 0 || timeout <= 0 {
		return 0
	}
	budgetNs := timeout.Nanoseconds() * int64(limits.CPUMillicores) / 1000
	return budgetNs / 100
}

// assign adds the process to the job. AssignProcessToJobObject
// requires PROCESS_SET_QUOTA and PROCESS_TERMINATE on the handle.
func (j *job) assign(pid int) error {
	h, err := xwin.OpenProcess(
		xwin.PROCESS_SET_QUOTA|xwin.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return errdefs.Internal(fmt.Errorf("windows: open process %d: %w", pid, err))
	}
	defer func() { _ = xwin.CloseHandle(h) }()
	if err := xwin.AssignProcessToJobObject(j.h, h); err != nil {
		return errdefs.Internal(fmt.Errorf("windows: assign process %d to job: %w", pid, err))
	}
	return nil
}

// terminate stops every process in the job. Windows has no SIGTERM:
// this is the only whole-tree stop and it is immediate. A job that
// already has no processes reports ERROR_ACCESS_DENIED, which is a
// successful no-op.
func (j *job) terminate() error {
	// Held across the syscall: close marks the handle released under the same
	// mutex, so a cap goroutine that wakes up as the session ends either kills a
	// live job or returns here — it can never terminate through a handle that has
	// been closed, and a recycled handle value cannot become another session's
	// job. terminate is also the path Close and Terminate use, which is why the
	// guard belongs here rather than in the caller.
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	if err := xwin.TerminateJobObject(j.h, 1); err != nil {
		if errors.Is(err, xwin.ERROR_ACCESS_DENIED) {
			return nil
		}
		return errdefs.Internal(fmt.Errorf("windows: terminate job: %w", err))
	}
	return nil
}

// close releases the job handle. With KILL_ON_JOB_CLOSE set, closing
// the last handle terminates all remaining processes.
func (j *job) close() error {
	if j.notify != nil {
		j.notify.stop()
	}
	// The flag and the handle go in one critical section. Marking first and
	// closing later leaves a window where the sampler has decided it may query and
	// then queries a handle this call has already released — possibly recycled into
	// another session's job. See terminate, which holds the same lock across its
	// syscall.
	j.mu.Lock()
	defer j.mu.Unlock()
	j.closed = true
	return xwin.CloseHandle(j.h)
}

// resumeProcess resumes every suspended thread of pid. The process is
// created with CREATE_SUSPENDED so it can be assigned to the job
// before any user code runs; only the primary thread starts
// suspended, so resuming all matching threads is safe (ResumeThread
// on an already-running thread is a no-op).
func resumeProcess(pid int) error {
	snapshot, err := xwin.CreateToolhelp32Snapshot(xwin.TH32CS_SNAPTHREAD, uint32(pid))
	if err != nil {
		return errdefs.Internal(fmt.Errorf("windows: snapshot threads of %d: %w", pid, err))
	}
	defer func() { _ = xwin.CloseHandle(snapshot) }()

	var entry xwin.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := xwin.Thread32First(snapshot, &entry); err != nil {
		return errdefs.Internal(fmt.Errorf("windows: first thread of %d: %w", pid, err))
	}
	for {
		if int(entry.OwnerProcessID) == pid && entry.ThreadID != 0 {
			if err := resumeThread(entry.ThreadID); err != nil {
				return err
			}
		}
		if err := xwin.Thread32Next(snapshot, &entry); err != nil {
			if errors.Is(err, xwin.ERROR_NO_MORE_FILES) {
				return nil
			}
			return errdefs.Internal(fmt.Errorf("windows: next thread of %d: %w", pid, err))
		}
	}
}

func resumeThread(tid uint32) error {
	h, err := xwin.OpenThread(xwin.THREAD_SUSPEND_RESUME, false, tid)
	if err != nil {
		return errdefs.Internal(fmt.Errorf("windows: open thread %d: %w", tid, err))
	}
	defer func() { _ = xwin.CloseHandle(h) }()
	if _, err := xwin.ResumeThread(h); err != nil {
		return errdefs.Internal(fmt.Errorf("windows: resume thread %d: %w", tid, err))
	}
	return nil
}
