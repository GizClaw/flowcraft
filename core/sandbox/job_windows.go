//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/GizClaw/flowcraft/core/telemetry"

	otellog "go.opentelemetry.io/otel/log"
	"golang.org/x/sys/windows"
)

// Job completion-port message constants (JOB_OBJECT_MSG_*). The kernel
// posts these when a hard job limit trips; we read them off the
// completion port associated with the job to classify the death.
const (
	jobMsgProcessMemoryLimit = 7
	jobMsgJobMemoryLimit     = 8
)

// jobWatchInterval is the cpu-time sampling period, matching the unix
// group watcher so both backends enforce with the same granularity.
const jobWatchInterval = 250 * time.Millisecond

// maxJobSampleFailures is how many consecutive sampling errors the
// watcher tolerates before declaring the caps unenforceable, mirroring
// the unix group watcher's maxSampleFailures.
const maxJobSampleFailures = 3

// jobObject owns one job object handle: every process the sandbox
// spawns is assigned to it (atomically, via the JOB_LIST process
// thread attribute), so Terminate/Close kill the whole tree and
// KILL_ON_JOB_CLOSE prevents leaks when the runner goes away. It is
// the Windows replacement for the unix process-group primitives
// (Setpgid / kill(-pgid)).
type jobObject struct {
	handle    windows.Handle
	closeOnce sync.Once
}

// newJobObject creates an unnamed job object with KILL_ON_JOB_CLOSE
// (last handle close kills every member) and BREAKAWAY_OK (children
// that already belong to an outer job — e.g. an IDE runner — may
// break away instead of failing to spawn).
func newJobObject() (*jobObject, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("sandbox: create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags =
		windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	if err := setJobLimits(h, &info); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	return &jobObject{handle: h}, nil
}

func setJobLimits(h windows.Handle, info *windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION) error {
	if _, err := windows.SetInformationJobObject(
		h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(info)),
		uint32(unsafe.Sizeof(*info)),
	); err != nil {
		return fmt.Errorf("sandbox: set job limits: %w", err)
	}
	return nil
}

// SetMemoryLimit applies the JOB_OBJECT_LIMIT_JOB_MEMORY hard cap:
// allocations over the limit fail at the kernel and a
// JOB_OBJECT_MSG_JOB_MEMORY_LIMIT notification is posted to the job's
// completion port, which the watcher turns into SessionBudgetExceeded.
func (j *jobObject) SetMemoryLimit(bytes int64) error {
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags =
		windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
			windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK |
			windows.JOB_OBJECT_LIMIT_JOB_MEMORY
	info.JobMemoryLimit = uintptr(bytes)
	return setJobLimits(j.handle, &info)
}

// Terminate hard-kills every process in the job. Idempotent in
// effect: terminating an already-empty job is a no-op success.
func (j *jobObject) Terminate() error {
	if err := windows.TerminateJobObject(j.handle, 1); err != nil {
		return fmt.Errorf("sandbox: terminate job: %w", err)
	}
	return nil
}

// Close releases the job handle. Safe to call more than once.
func (j *jobObject) Close() error {
	j.closeOnce.Do(func() {
		_ = windows.CloseHandle(j.handle)
	})
	return nil
}

// jobBasicProcessIDList mirrors JOBOBJECT_BASIC_PROCESS_ID_LIST.
type jobBasicProcessIDList struct {
	numberOfAssignedProcesses uint32
	numberOfProcessIDsInList  uint32
	processIDList             [1]uint32
}

// processIDs lists the current members of the job.
func (j *jobObject) processIDs() ([]uint32, error) {
	var header jobBasicProcessIDList
	var needed uint32
	// First query with the minimal buffer just learns the required
	// size; ERROR_MORE_DATA is expected and ignored.
	_ = windows.QueryInformationJobObject(
		j.handle,
		windows.JobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&header)),
		uint32(unsafe.Sizeof(header)),
		&needed,
	)
	if needed <= uint32(unsafe.Sizeof(header)) {
		// The header alone was returned: the job is empty.
		if needed == 0 {
			return nil, nil
		}
		needed = uint32(unsafe.Sizeof(header))
	}
	buf := make([]byte, needed)
	if err := windows.QueryInformationJobObject(
		j.handle,
		windows.JobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&buf[0])),
		needed,
		&needed,
	); err != nil {
		return nil, fmt.Errorf("sandbox: query job process list: %w", err)
	}
	numIDs := *(*uint32)(unsafe.Pointer(&buf[4]))
	pids := make([]uint32, 0, numIDs)
	for i := 0; i < int(numIDs); i++ {
		pids = append(pids, *(*uint32)(unsafe.Pointer(&buf[8+i*4])))
	}
	return pids, nil
}

// sampleCPU sums kernel+user cpu time across every process currently
// in the job (100ns units, like GetProcessTimes), matching the unix
// group sampler's aggregate-cpu-time semantics.
func (j *jobObject) sampleCPU() (time.Duration, error) {
	pids, err := j.processIDs()
	if err != nil {
		return 0, err
	}
	var units uint64
	for _, pid := range pids {
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if err != nil {
			// The process exited between enumeration and open; it can
			// no longer consume cpu time, so skipping is correct.
			continue
		}
		var ct, et, kt, ut windows.Filetime
		err = windows.GetProcessTimes(h, &ct, &et, &kt, &ut)
		_ = windows.CloseHandle(h)
		if err != nil {
			continue
		}
		units += uint64(kt.HighDateTime)<<32 | uint64(kt.LowDateTime)
		units += uint64(ut.HighDateTime)<<32 | uint64(ut.LowDateTime)
	}
	return time.Duration(units * 100), nil
}

// jobCapsWatcher enforces MemoryBytes (hard job limit + completion
// port notifications) and CPUMillicores (sampled cpu-time budget) and
// kills the job on overflow, mirroring GroupCapsWatcher's contract:
// Stop after reaping, consult Unenforceable before Exceeded.
type jobCapsWatcher struct {
	ctx       context.Context
	job       *jobObject
	port      windows.Handle
	maxCPU    time.Duration
	stopCh    chan struct{}
	doneCh    chan struct{}
	stopOnce  sync.Once
	portOnce  sync.Once
	exceeded  atomic.Value // string
	sampleErr atomic.Value // error
}

// startJobCapsWatcher launches enforcement for job. It returns nil
// when neither cap is actionable, so callers may invoke Stop
// unconditionally. Memory is enforced as a kernel hard limit set
// before spawn; the watcher's port classifies the kill.
func startJobCapsWatcher(ctx context.Context, job *jobObject, res ResourceLimits, timeout time.Duration) *jobCapsWatcher {
	_, maxCPU := deriveGroupCaps(res, timeout)
	if maxCPU == 0 && res.MemoryBytes <= 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w := &jobCapsWatcher{
		ctx:    ctx,
		job:    job,
		maxCPU: maxCPU,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	if res.MemoryBytes > 0 {
		// CreateIoCompletionPort with a zero port handle both creates
		// the port and associates the job with it.
		port, err := windows.CreateIoCompletionPort(job.handle, 0, 0, 0)
		if err != nil {
			wrapped := fmt.Errorf("sandbox: associate job completion port: %w", err)
			w.sampleErr.Store(wrapped)
		} else {
			w.port = port
		}
	}
	go w.run()
	return w
}

func (w *jobCapsWatcher) run() {
	defer close(w.doneCh)
	if w.sampleErr.Load() != nil {
		// The memory cap cannot be observed; the job was spawned under
		// the hard limit but death-by-budget would be unclassifiable.
		// Fail closed rather than pretend the guard exists.
		w.kill("sample_failure")
		return
	}
	// CPU sampling only guards a configured cpu-time budget. A
	// memory-only job must not be killed because the cpu sampler broke
	// down: its memory cap is a kernel hard limit that does not depend
	// on sampling at all. Completion-port errors below still count,
	// because the port is what makes the memory cap observable.
	var ticker *time.Ticker
	if w.maxCPU > 0 {
		ticker = time.NewTicker(jobWatchInterval)
		defer ticker.Stop()
	}
	failures := 0
	for {
		select {
		case <-w.stopCh:
			return
		default:
		}
		if ticker != nil {
			select {
			case <-ticker.C:
				cpu, err := w.job.sampleCPU()
				if err != nil {
					failures++
					if failures < maxJobSampleFailures {
						continue
					}
					wrapped := fmt.Errorf("job cpu sampling failed %d times in a row: %w", failures, err)
					w.sampleErr.Store(wrapped)
					telemetry.WarnErr(w.ctx, "sandbox: cpu sampling failed; killing job", wrapped,
						otellog.String("sandbox.kill_reason", "sample_failure"))
					w.kill("sample_failure")
					return
				}
				failures = 0
				if cpu >= w.maxCPU {
					w.exceeded.Store("cpu-time")
					w.kill("cpu_cap")
					return
				}
			default:
			}
		}
		if w.port != 0 {
			var qty uint32
			var key uintptr
			var ovl *windows.Overlapped
			err := windows.GetQueuedCompletionStatus(
				w.port, &qty, &key, &ovl, uint32(jobWatchInterval/time.Millisecond))
			if err != nil {
				if err == windows.ERROR_TIMEOUT {
					continue
				}
				failures++
				if failures < maxJobSampleFailures {
					continue
				}
				wrapped := fmt.Errorf("job completion port failed %d times in a row: %w", failures, err)
				w.sampleErr.Store(wrapped)
				telemetry.WarnErr(w.ctx, "sandbox: job completion port failed; killing job", wrapped,
					otellog.String("sandbox.kill_reason", "sample_failure"))
				w.kill("sample_failure")
				return
			}
			switch qty {
			case jobMsgJobMemoryLimit, jobMsgProcessMemoryLimit:
				w.exceeded.Store("memory")
				w.kill("memory_cap")
				return
			}
		}
	}
}

func (w *jobCapsWatcher) kill(reason string) {
	if err := w.job.Terminate(); err != nil {
		telemetry.WarnErr(w.ctx,
			"sandbox: failed to terminate job after resource cap",
			err,
			otellog.String("sandbox.kill_reason", reason))
	}
}

// Stop ends enforcement and waits for the watcher goroutine to exit.
// It is nil-safe and safe to call from multiple goroutines: the stop
// signal and the completion-port handle are each released exactly
// once, so a concurrent Stop from Close and reap cannot double-close
// the port.
func (w *jobCapsWatcher) Stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() { close(w.stopCh) })
	<-w.doneCh
	w.portOnce.Do(func() {
		if w.port != 0 {
			_ = windows.CloseHandle(w.port)
		}
	})
}

// Unenforceable returns a non-nil error when the watcher gave up on
// observing the caps and killed the job. Consult it before Exceeded.
func (w *jobCapsWatcher) Unenforceable() error {
	if w == nil {
		return nil
	}
	if p := w.sampleErr.Load(); p != nil {
		return p.(error)
	}
	return nil
}

// Exceeded reports which configured cap terminated the job, or "".
func (w *jobCapsWatcher) Exceeded() string {
	if w == nil {
		return ""
	}
	if p := w.exceeded.Load(); p != nil {
		return p.(string)
	}
	return ""
}
