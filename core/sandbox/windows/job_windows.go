//go:build windows

package windows

import (
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	xwin "golang.org/x/sys/windows"
)

// job wraps a Windows Job Object. A job is the kernel-level process
// group: processes assigned to it are managed as a unit, so
// terminating the job kills every descendant (not just the leader)
// and job limits apply across the whole tree.
type job struct {
	h      xwin.Handle
	notify *jobNotifier // nil when no resource caps are configured
}

// Job completion-port message codes (winnt.h).
const (
	jobObjectMsgEndOfProcessTime = 3
	jobObjectMsgJobMemoryLimit   = 10

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

// jobNotifier drains a job object's completion port and records which
// resource cap the kernel enforced, so a cap kill can be classified as
// SessionBudgetExceeded instead of a plain exit.
type jobNotifier struct {
	port   xwin.Handle
	done   chan struct{}
	mu     sync.Mutex
	budget string // "memory" or "cpu"; first cap wins
}

func (n *jobNotifier) setBudget(cap string) {
	n.mu.Lock()
	if n.budget == "" {
		n.budget = cap
	}
	n.mu.Unlock()
}

func (n *jobNotifier) budgetCap() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.budget
}

// run drains completion messages until stop posts the sentinel or the
// port is closed.
func (n *jobNotifier) run() {
	defer close(n.done)
	for {
		var qty uint32
		var key uintptr
		var ovl *xwin.Overlapped
		if err := xwin.GetQueuedCompletionStatus(n.port, &qty, &key, &ovl, xwin.INFINITE); err != nil {
			return
		}
		if key == notifierStop {
			return
		}
		switch qty {
		case jobObjectMsgJobMemoryLimit:
			n.setBudget("memory")
		case jobObjectMsgEndOfProcessTime:
			n.setBudget("cpu")
		}
	}
}

// stop posts the sentinel, waits for the drain goroutine (bounded),
// and closes the port.
func (n *jobNotifier) stop() {
	if n == nil {
		return
	}
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
func newJobNotifier(jobHandle xwin.Handle) (*jobNotifier, error) {
	port, err := xwin.CreateIoCompletionPort(xwin.InvalidHandle, 0, 0, 1)
	if err != nil {
		return nil, errdefs.Internal(fmt.Errorf("windows: create job completion port: %w", err))
	}
	n := &jobNotifier{
		port: port,
		done: make(chan struct{}),
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
	j := &job{h: h}
	info := jobLimits(limits, timeout)
	if _, err := xwin.SetInformationJobObject(
		h, xwin.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = j.close()
		return nil, errdefs.Internal(fmt.Errorf("windows: set job limits: %w", err))
	}
	if limits.MemoryBytes > 0 || limits.CPUMillicores > 0 {
		notify, err := newJobNotifier(h)
		if err != nil {
			_ = j.close()
			return nil, err
		}
		j.notify = notify
	}
	return j, nil
}

// budgetCap reports which resource cap the kernel enforced, if any.
func (j *job) budgetCap() string {
	if j.notify == nil {
		return ""
	}
	return j.notify.budgetCap()
}

// jobLimits maps sandbox.ResourceLimits onto the Windows extended
// limit structure. MemoryBytes becomes a job-wide cap (the same
// "aggregate process group" semantics as the unix watcher);
// CPUMillicores becomes a per-process user-time cap derived from
// Timeout x millicores/1000 (the same budget the unix watcher
// enforces, but enforced by the kernel per process rather than
// sampled across the group).
func jobLimits(limits sandbox.ResourceLimits, timeout time.Duration) xwin.JOBOBJECT_EXTENDED_LIMIT_INFORMATION {
	var info xwin.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags |= xwin.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if limits.MemoryBytes > 0 {
		info.BasicLimitInformation.LimitFlags |= xwin.JOB_OBJECT_LIMIT_JOB_MEMORY
		info.JobMemoryLimit = uintptr(limits.MemoryBytes)
	}
	if limits.CPUMillicores > 0 {
		info.BasicLimitInformation.LimitFlags |= xwin.JOB_OBJECT_LIMIT_PROCESS_TIME
		info.BasicLimitInformation.PerProcessUserTimeLimit = cpuBudget100ns(limits, timeout)
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
