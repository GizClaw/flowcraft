//go:build windows

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
	"unsafe"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"github.com/rs/xid"
	otellog "go.opentelemetry.io/otel/log"
	"golang.org/x/sys/windows"
)

const (
	defaultSessionRows = 24
	defaultSessionCols = 80
	// sessionCopyChunk bounds each copy-loop read; the output log's
	// trim keeps a chunk larger than the budget replayable.
	sessionCopyChunk = 32 * 1024
	// sessionWriteConcurrency bounds concurrent Write calls so a
	// blocked child cannot be flooded by parallel producers.
	sessionWriteConcurrency = 4
	// sessionKillTimeout bounds how long Close waits for the job to
	// die after TerminateJobObject.
	sessionKillTimeout = 2 * time.Second
	// windowsTerminateGrace is how long Terminate waits after closing
	// the child's stdin before escalating to TerminateJobObject, so
	// console children get a chance to flush and exit on EOF.
	windowsTerminateGrace = 1 * time.Second
	// pseudoConsoleResizeQuirk is the ConPTY flag that lets the
	// pseudoconsole report resize events to the attached console.
	pseudoConsoleResizeQuirk = 0x2
)

// Process-thread attribute identifiers (winnt.h authoritative values;
// x/sys's HANDLE_LIST constant is not authoritative, so define all
// three locally).
const (
	procThreadAttributeHandleList = 0x00020003
	procThreadAttributeJobList    = 0x0002000d
	procThreadAttrPseudoConsole   = 0x00020016
)

// StartWindowsSession launches an already-configured *exec.Cmd as a
// Session. cmd.Dir / cmd.Env must already be resolved by the caller,
// and the restricted token must be attached through
// cmd.SysProcAttr.Token (the core/sandbox/windows backend does both).
// StartWindowsSession owns the stdio plumbing and lifecycle only:
//
//   - tty=true: a ConPTY becomes the child's console; stdout/stderr
//     are merged into SessionStreamTTY, Resize maps to
//     ResizePseudoConsole, and Ctrl-C is delivered as the VINTR byte
//     through the console input pipe.
//   - tty=false: stdin is piped and stdout/stderr are tagged streams.
//
// Policy validation belongs to the backend; this constructor enforces
// mechanics only. The child is assigned to a Job Object atomically at
// spawn (JOB_LIST process-thread attribute), so no window exists in
// which the tree is uncontained. Memory/CPU caps ride the job's hard
// limit plus a completion-port/sampling watcher, and reaping follows
// the same seq-cursor output-log contract as StartSession on unix.
func StartWindowsSession(ctx context.Context, spec SessionSpec, cmd *exec.Cmd) (Session, error) {
	if cmd == nil {
		return nil, errdefs.Validationf("sandbox: nil command for process session")
	}
	rows, cols := spec.Rows, spec.Cols
	if rows <= 0 {
		rows = defaultSessionRows
	}
	if cols <= 0 {
		cols = defaultSessionCols
	}
	id := spec.ID
	if id == "" {
		id = xid.New().String()
	}

	token := windows.Token(0)
	if cmd.SysProcAttr != nil {
		token = windows.Token(cmd.SysProcAttr.Token)
	}
	if token == 0 {
		return nil, errdefs.NotAvailablef(
			"sandbox: windows session requires a restricted token attached to cmd.SysProcAttr.Token")
	}

	s := &windowsSession{
		id:         id,
		token:      token,
		argv:       cmd.Args,
		dir:        cmd.Dir,
		env:        cmd.Env,
		tty:        spec.TTY,
		rows:       rows,
		cols:       cols,
		out:        newOutputLog(spec.Opts.Resources.MaxOutputBytes),
		done:       make(chan struct{}),
		writeSlots: make(chan struct{}, sessionWriteConcurrency),
	}

	job, err := newJobObject()
	if err != nil {
		return nil, err
	}
	s.job = job
	if spec.Opts.Resources.MemoryBytes > 0 {
		if err := job.SetMemoryLimit(spec.Opts.Resources.MemoryBytes); err != nil {
			_ = job.Close()
			return nil, err
		}
	}
	if spec.Opts.Resources.MemoryBytes > 0 || spec.Opts.Resources.CPUMillicores > 0 {
		// Start the watcher before spawn so the completion port is
		// associated before any process can trip the memory cap (a
		// message posted before association would be lost). The Start
		// ctx only bounds the spawn; WithoutCancel keeps the watcher's
		// telemetry rooted in the originating trace without making a
		// canceled Start kill the session.
		s.watcher = startJobCapsWatcher(context.WithoutCancel(ctx), s.job, spec.Opts.Resources, spec.Opts.Timeout)
	}

	if spec.TTY {
		if err := s.spawnConPTY(); err != nil {
			if s.watcher != nil {
				s.watcher.Stop()
			}
			_ = job.Close()
			return nil, err
		}
	} else {
		if err := s.spawnPipes(); err != nil {
			if s.watcher != nil {
				s.watcher.Stop()
			}
			_ = job.Close()
			return nil, err
		}
	}

	if spec.Opts.Timeout > 0 {
		timer := time.AfterFunc(spec.Opts.Timeout, s.timeoutKill)
		go func() {
			<-s.done
			timer.Stop()
		}()
	}
	go s.reap()
	return s, nil
}

// windowsSession is the concrete Windows backend session: it owns the
// child's stdio (pipes or ConPTY), the job object, the resource-cap
// watcher, and the shared replayable output log.
type windowsSession struct {
	id    string
	token windows.Token
	argv  []string
	dir   string
	env   []string
	tty   bool
	rows  int
	cols  int

	job        *jobObject
	proc       windows.Handle
	pid        uint32
	ptty       windows.Handle // pseudoconsole (0 for pipe sessions)
	conIn      windows.Handle // pseudoconsole input pipe, borrowed by ConPTY
	conOut     windows.Handle // pseudoconsole output pipe, borrowed by ConPTY
	stdin      *os.File
	stdout     *os.File
	stderr     *os.File
	out        *outputLog
	watcher    *jobCapsWatcher
	copiers    sync.WaitGroup
	writeSlots chan struct{}

	mu         sync.Mutex
	closed     bool
	timedOut   bool
	terminated bool
	exit       SessionExit
	waitErr    error
	done       chan struct{}
}

// spawnPipes launches a non-TTY child with three anonymous pipes and
// attaches it to the job atomically via the JOB_LIST attribute.
func (s *windowsSession) spawnPipes() error {
	var inRead, inWrite, outRead, outWrite, errRead, errWrite windows.Handle
	fail := func(err error) error {
		closeHandles(inRead, inWrite, outRead, outWrite, errRead, errWrite)
		return err
	}
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return fail(fmt.Errorf("sandbox: create stdin pipe: %w", err))
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		return fail(fmt.Errorf("sandbox: create stdout pipe: %w", err))
	}
	if err := windows.CreatePipe(&errRead, &errWrite, nil, 0); err != nil {
		return fail(fmt.Errorf("sandbox: create stderr pipe: %w", err))
	}
	childHandles := []windows.Handle{inRead, outWrite, errWrite}
	for _, h := range childHandles {
		if err := windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return fail(fmt.Errorf("sandbox: make stdio handle inheritable: %w", err))
		}
	}

	attrs, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return fail(fmt.Errorf("sandbox: init process thread attribute list: %w", err))
	}
	defer attrs.Delete()
	jobList := []windows.Handle{s.job.handle}
	if err := attrs.Update(procThreadAttributeJobList, unsafe.Pointer(&jobList[0]), uintptr(len(jobList))*unsafe.Sizeof(jobList[0])); err != nil {
		return fail(fmt.Errorf("sandbox: set job list attribute: %w", err))
	}
	if err := attrs.Update(procThreadAttributeHandleList, unsafe.Pointer(&childHandles[0]), uintptr(len(childHandles))*unsafe.Sizeof(childHandles[0])); err != nil {
		return fail(fmt.Errorf("sandbox: set handle list attribute: %w", err))
	}

	cmdline, err := s.commandLine()
	if err != nil {
		return fail(err)
	}
	envBlock := makeEnvBlock(s.env)
	desktop, err := windows.UTF16PtrFromString(`Winsta0\Default`)
	if err != nil {
		return fail(fmt.Errorf("sandbox: encode desktop name: %w", err))
	}
	cwd, err := windows.UTF16PtrFromString(s.dir)
	if err != nil {
		return fail(fmt.Errorf("sandbox: encode cwd: %w", err))
	}

	var si windows.StartupInfoEx
	si.StartupInfo.Cb = uint32(unsafe.Sizeof(si))
	si.StartupInfo.Desktop = desktop
	si.StartupInfo.Flags |= windows.STARTF_USESTDHANDLES
	si.StartupInfo.StdInput = inRead
	si.StartupInfo.StdOutput = outWrite
	si.StartupInfo.StdErr = errWrite
	si.ProcThreadAttributeList = attrs.List()

	var pi windows.ProcessInformation
	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_NO_WINDOW)
	if err := windows.CreateProcessAsUser(
		s.token,
		nil,
		&cmdline[0],
		nil,
		nil,
		true,
		flags,
		&envBlock[0],
		cwd,
		&si.StartupInfo,
		&pi,
	); err != nil {
		return fail(classifyWindowsStartError(s.argv[0], err))
	}
	_ = windows.CloseHandle(pi.Thread)

	// The child holds duplicates of the read/write ends now; drop ours.
	_ = windows.CloseHandle(inRead)
	_ = windows.CloseHandle(outWrite)
	_ = windows.CloseHandle(errWrite)

	s.proc = pi.Process
	s.pid = pi.ProcessId
	s.stdin = os.NewFile(uintptr(inWrite), "sandbox-stdin")
	s.stdout = os.NewFile(uintptr(outRead), "sandbox-stdout")
	s.stderr = os.NewFile(uintptr(errRead), "sandbox-stderr")

	s.copiers.Add(2)
	go s.copyStream(s.stdout, SessionStreamStdout)
	go s.copyStream(s.stderr, SessionStreamStderr)
	return nil
}

// spawnConPTY launches a TTY child attached to a pseudoconsole. The
// ConPTY borrows inRead/outWrite for its lifetime, so those handles
// stay open until ClosePseudoConsole; the host reads outRead and
// writes inWrite.
func (s *windowsSession) spawnConPTY() error {
	var inRead, inWrite, outRead, outWrite windows.Handle
	fail := func(err error) error {
		closeHandles(inRead, inWrite, outRead, outWrite)
		return err
	}
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return fail(fmt.Errorf("sandbox: create conpty input pipe: %w", err))
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		return fail(fmt.Errorf("sandbox: create conpty output pipe: %w", err))
	}
	var hpc windows.Handle
	if err := windows.CreatePseudoConsole(
		windows.Coord{X: int16(s.cols), Y: int16(s.rows)},
		inRead,
		outWrite,
		pseudoConsoleResizeQuirk,
		&hpc,
	); err != nil {
		return fail(fmt.Errorf("sandbox: create pseudo console: %w", err))
	}
	s.ptty = hpc
	s.conIn = inRead
	s.conOut = outWrite
	// Ownership of inRead/outWrite now belongs to the pseudoconsole
	// (closeConPTY releases them). Zero the locals so the fail closure
	// cannot double-close them on a later error path — on Windows a
	// closed handle value can be reused by another object, and a
	// second CloseHandle would tear down an unrelated handle.
	inRead, outWrite = 0, 0

	attrs, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		s.closeConPTY()
		return fail(fmt.Errorf("sandbox: init process thread attribute list: %w", err))
	}
	defer attrs.Delete()
	jobList := []windows.Handle{s.job.handle}
	if err := attrs.Update(procThreadAttributeJobList, unsafe.Pointer(&jobList[0]), uintptr(len(jobList))*unsafe.Sizeof(jobList[0])); err != nil {
		s.closeConPTY()
		return fail(fmt.Errorf("sandbox: set job list attribute: %w", err))
	}
	// PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE's lpValue is the handle value
	// itself (see the Microsoft ConPTY sample); reinterpret the
	// uintptr's storage so vet's unsafeptr check stays quiet.
	hpcValue := *(*unsafe.Pointer)(unsafe.Pointer(&hpc))
	if err := attrs.Update(procThreadAttrPseudoConsole, hpcValue, unsafe.Sizeof(hpc)); err != nil {
		s.closeConPTY()
		return fail(fmt.Errorf("sandbox: set pseudo console attribute: %w", err))
	}

	cmdline, err := s.commandLine()
	if err != nil {
		s.closeConPTY()
		return fail(err)
	}
	envBlock := makeEnvBlock(s.env)
	desktop, err := windows.UTF16PtrFromString(`Winsta0\Default`)
	if err != nil {
		s.closeConPTY()
		return fail(fmt.Errorf("sandbox: encode desktop name: %w", err))
	}
	cwd, err := windows.UTF16PtrFromString(s.dir)
	if err != nil {
		s.closeConPTY()
		return fail(fmt.Errorf("sandbox: encode cwd: %w", err))
	}

	var si windows.StartupInfoEx
	si.StartupInfo.Cb = uint32(unsafe.Sizeof(si))
	si.StartupInfo.Desktop = desktop
	// The pseudoconsole provides the child's console; stdio handles are
	// explicitly invalid so nothing else is inherited.
	si.StartupInfo.Flags |= windows.STARTF_USESTDHANDLES
	si.StartupInfo.StdInput = windows.InvalidHandle
	si.StartupInfo.StdOutput = windows.InvalidHandle
	si.StartupInfo.StdErr = windows.InvalidHandle
	si.ProcThreadAttributeList = attrs.List()

	var pi windows.ProcessInformation
	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT)
	if err := windows.CreateProcessAsUser(
		s.token,
		nil,
		&cmdline[0],
		nil,
		nil,
		false,
		flags,
		&envBlock[0],
		cwd,
		&si.StartupInfo,
		&pi,
	); err != nil {
		s.closeConPTY()
		return fail(classifyWindowsStartError(s.argv[0], err))
	}
	_ = windows.CloseHandle(pi.Thread)

	s.proc = pi.Process
	s.pid = pi.ProcessId
	s.stdin = os.NewFile(uintptr(inWrite), "sandbox-conpty-stdin")
	s.stdout = os.NewFile(uintptr(outRead), "sandbox-conpty-stdout")

	s.copiers.Add(1)
	go s.copyStream(s.stdout, SessionStreamTTY)
	return nil
}

// commandLine builds the CreateProcess command line from the argv the
// backend configured. argv[0] is quoted like any other argument.
func (s *windowsSession) commandLine() ([]uint16, error) {
	if len(s.argv) == 0 {
		return nil, errdefs.Validationf("sandbox: SessionSpec.Argv must name a command")
	}
	return windows.UTF16FromString(argvToCommandLine(s.argv))
}

func (s *windowsSession) copyStream(r io.Reader, stream SessionStream) {
	defer s.copiers.Done()
	buf := make([]byte, sessionCopyChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.out.append(stream, buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (s *windowsSession) ID() string { return s.id }

func (s *windowsSession) PID() int { return int(s.pid) }

func (s *windowsSession) Read(ctx context.Context, afterSeq int64, maxBytes int) (SessionOutput, error) {
	if s.isClosed() {
		return SessionOutput{}, ErrSessionClosed
	}
	return s.out.read(ctx, afterSeq, maxBytes)
}

func (s *windowsSession) Write(ctx context.Context, data []byte) error {
	if s.isClosed() {
		return ErrSessionClosed
	}
	s.mu.Lock()
	stdin := s.stdin
	s.mu.Unlock()
	if stdin == nil {
		return ErrSessionClosed
	}
	if len(data) == 0 {
		return nil
	}
	select {
	case s.writeSlots <- struct{}{}:
	case <-ctx.Done():
		return errdefs.FromContext(ctx.Err())
	case <-s.done:
		return ErrSessionClosed
	}
	defer func() { <-s.writeSlots }()
	for len(data) > 0 {
		n, err := stdin.Write(data)
		if err != nil {
			if errors.Is(err, os.ErrClosed) || errors.Is(err, windows.ERROR_BROKEN_PIPE) {
				return ErrSessionClosed
			}
			return errdefs.Internal(fmt.Errorf("sandbox: write session stdin: %w", err))
		}
		data = data[n:]
	}
	return nil
}

// CloseInput closes the session's stdin. TTY sessions cannot close
// their input (the ConPTY is bidirectional) and return NotAvailable.
func (s *windowsSession) CloseInput() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrSessionClosed
	}
	if s.tty {
		s.mu.Unlock()
		return errdefs.NotAvailablef(
			"sandbox: cannot close input on a TTY session")
	}
	s.mu.Unlock()
	s.closeInput()
	return nil
}

func (s *windowsSession) Resize(_ context.Context, rows, cols int) error {
	if rows <= 0 || cols <= 0 {
		return errdefs.Validationf("sandbox: rows and cols must be positive")
	}
	if s.isClosed() {
		return ErrSessionClosed
	}
	s.mu.Lock()
	ptty := s.ptty
	s.mu.Unlock()
	if ptty == 0 {
		return errdefs.NotAvailablef("sandbox: Resize requires a TTY session")
	}
	if err := windows.ResizePseudoConsole(ptty, windows.Coord{X: int16(cols), Y: int16(rows)}); err != nil {
		return errdefs.Internal(fmt.Errorf("sandbox: resize pseudo console: %w", err))
	}
	return nil
}

func (s *windowsSession) Terminate(ctx context.Context) error {
	if s.isClosed() {
		return ErrSessionClosed
	}
	select {
	case <-s.done:
		return nil
	default:
	}
	s.mu.Lock()
	s.terminated = true
	s.mu.Unlock()
	// EOF on stdin gives console children a chance to flush and exit.
	s.closeInput()

	grace := windowsTerminateGrace
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < grace {
			grace = remaining
		}
	}
	if grace <= 0 {
		s.killJob("sandbox: terminate job after zero grace failed")
		return ctx.Err()
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-s.done:
		return nil
	case <-timer.C:
		s.killJob("sandbox: terminate job on grace timeout failed")
		return nil
	case <-ctx.Done():
		s.killJob("sandbox: terminate job on cancel failed")
		return ctx.Err()
	}
}

// Signal implements Session: interrupt means Ctrl-C semantics — the
// VINTR byte through the ConPTY input on TTY sessions (the console
// driver signals the foreground process group). Pipe sessions have no
// console to signal through and return NotAvailable.
func (s *windowsSession) Signal(_ context.Context, sig SessionSignal) error {
	if sig != SessionSignalInterrupt {
		return errdefs.NotAvailablef("sandbox: signal %v not supported", sig)
	}
	if s.isClosed() {
		return ErrSessionClosed
	}
	if !s.tty {
		return errdefs.NotAvailablef(
			"sandbox: Signal requires a TTY session on windows")
	}
	s.mu.Lock()
	stdin := s.stdin
	s.mu.Unlock()
	if stdin == nil {
		return ErrSessionClosed
	}
	if _, err := stdin.Write([]byte{0x03}); err != nil {
		return errdefs.Internal(fmt.Errorf("sandbox: signal write to conpty: %w", err))
	}
	return nil
}

// Watch implements Session using the shared replay-then-live queue.
func (s *windowsSession) Watch(ctx context.Context) (SessionWatcher, error) {
	if s.isClosed() {
		return nil, ErrSessionClosed
	}
	return watchOutputLog(ctx, s.out)
}

// Capabilities declares this session's actual surface: TTY and Signal
// only for ConPTY sessions, Events on every session.
func (s *windowsSession) Capabilities() SessionCapabilities {
	return SessionCapabilities{TTY: s.tty, Signal: s.tty, Events: true}
}

func (s *windowsSession) Wait(ctx context.Context) (SessionExit, error) {
	select {
	case <-s.done:
	case <-ctx.Done():
		return SessionExit{}, ctx.Err()
	}
	s.mu.Lock()
	exit, err := s.exit, s.waitErr
	s.mu.Unlock()
	return exit, err
}

func (s *windowsSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	select {
	case <-s.done:
	default:
		s.killJob("sandbox: terminate job on close failed")
		select {
		case <-s.done:
		case <-time.After(sessionKillTimeout):
			telemetry.Warn(context.Background(),
				"sandbox: job did not exit after terminate on close",
				otellog.String("sandbox.session_id", s.id),
				otellog.Int("sandbox.pid", int(s.pid)))
		}
	}
	// reap owns the process handle and closes it after reaping, so
	// Close must never CloseHandle(proc) — a handle closed here while
	// reap still waits on it could race a reused handle value. Stop the
	// watcher before releasing the job so it never samples a handle we
	// are about to close; job.Close()'s KILL_ON_JOB_CLOSE then lets a
	// still-running reap's wait complete.
	if s.watcher != nil {
		s.watcher.Stop()
	}

	s.closeConPTY()
	s.closeInput()
	s.closeFiles()
	_ = s.job.Close()
	s.out.close()
	return nil
}

func (s *windowsSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// timeoutKill is the ExecOptions.Timeout enforcement: the job is
// terminated immediately, like Runner.Exec does on ctx deadline.
func (s *windowsSession) timeoutKill() {
	s.mu.Lock()
	s.timedOut = true
	s.mu.Unlock()
	s.killJob("sandbox: terminate job on timeout failed")
}

func (s *windowsSession) killJob(msg string) {
	if err := s.job.Terminate(); err != nil {
		telemetry.WarnErr(context.Background(), msg, err,
			otellog.String("sandbox.session_id", s.id),
			otellog.Int("sandbox.pid", int(s.pid)))
	}
}

func (s *windowsSession) closeInput() {
	s.mu.Lock()
	stdin := s.stdin
	s.stdin = nil
	s.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
}

func (s *windowsSession) closeFiles() {
	s.mu.Lock()
	stdout := s.stdout
	s.stdout = nil
	stderr := s.stderr
	s.stderr = nil
	s.mu.Unlock()
	if stdout != nil {
		_ = stdout.Close()
	}
	if stderr != nil {
		_ = stderr.Close()
	}
}

// closeConPTY releases the pseudoconsole and the pipe ends it
// borrowed. It also unblocks the output copier (the read end closes).
func (s *windowsSession) closeConPTY() {
	s.mu.Lock()
	ptty := s.ptty
	s.ptty = 0
	conIn := s.conIn
	s.conIn = 0
	conOut := s.conOut
	s.conOut = 0
	s.mu.Unlock()
	if ptty == 0 {
		return
	}
	windows.ClosePseudoConsole(ptty)
	closeHandles(conIn, conOut)
}

// reap waits for the child, stops the watcher, drains the copiers,
// classifies the exit, then finishes the output log.
func (s *windowsSession) reap() {
	proc := s.procHandle()
	if proc != 0 {
		if _, err := windows.WaitForSingleObject(proc, windows.INFINITE); err != nil {
			telemetry.WarnErr(context.Background(), "sandbox: wait for child process failed", err,
				otellog.String("sandbox.session_id", s.id))
		}
	}
	if s.watcher != nil {
		s.watcher.Stop()
	}
	// Close the pseudoconsole so the output copier sees EOF; then wait
	// for it so EOF is never reported early.
	s.closeConPTY()
	s.copiers.Wait()

	exit, err := s.classifyExit()
	s.mu.Lock()
	s.exit = exit
	s.waitErr = err
	s.mu.Unlock()
	// reap is the sole owner of the process handle: close it only now,
	// after classifyExit has read the exit code, so no other goroutine
	// (Close included) can race a closed handle value.
	if proc != 0 {
		_ = windows.CloseHandle(proc)
		s.mu.Lock()
		s.proc = 0
		s.mu.Unlock()
	}
	s.out.finish(s.exit)
	close(s.done)
}

func (s *windowsSession) classifyExit() (SessionExit, error) {
	if s.watcher != nil {
		// Order matters: a watcher that gave up on sampling killed the
		// job without proving any budget was exceeded.
		if sampleErr := s.watcher.Unenforceable(); sampleErr != nil {
			return SessionExit{Code: -1, Reason: SessionUnenforceable},
				errdefs.NotAvailablef(
					"sandbox: resource caps became unenforceable while running process: %v", sampleErr)
		}
		if capName := s.watcher.Exceeded(); capName != "" {
			return SessionExit{Code: -1, Reason: SessionBudgetExceeded},
				errdefs.BudgetExceededf(
					"sandbox: %s resource cap exceeded while running process", capName)
		}
	}

	s.mu.Lock()
	timedOut := s.timedOut
	terminated := s.terminated
	s.mu.Unlock()
	if timedOut {
		return SessionExit{Code: -1, Reason: SessionTimedOut},
			errdefs.FromContext(fmt.Errorf("sandbox: process exceeded its Timeout: %w", context.DeadlineExceeded))
	}
	var code uint32
	if proc := s.procHandle(); proc != 0 {
		if err := windows.GetExitCodeProcess(proc, &code); err != nil {
			return SessionExit{Code: -1, Reason: SessionExited},
				errdefs.Internal(fmt.Errorf("sandbox: get exit code: %w", err))
		}
	}
	if terminated {
		return SessionExit{Code: -1, Reason: SessionTerminated}, nil
	}
	return SessionExit{Code: int(code), Reason: SessionExited}, nil
}

func (s *windowsSession) procHandle() windows.Handle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proc
}

func classifyWindowsStartError(cmd string, err error) error {
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return errdefs.NotFound(fmt.Errorf("sandbox: exec %s: %w", cmd, err))
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return errdefs.Forbidden(fmt.Errorf("sandbox: exec %s: %w", cmd, err))
	}
	return errdefs.Internal(fmt.Errorf("sandbox: exec %s: %w", cmd, err))
}

func closeHandles(hs ...windows.Handle) {
	for _, h := range hs {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
	}
}

var _ Session = (*windowsSession)(nil)
