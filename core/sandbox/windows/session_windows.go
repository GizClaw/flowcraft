//go:build windows

package windows

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"

	otellog "go.opentelemetry.io/otel/log"
	xwin "golang.org/x/sys/windows"
)

const (
	sessionWriteConcurrency = 4
	// sessionKillTimeout bounds how long Close waits for the job to
	// die after TerminateJobObject. A stuck kernel state must not hang
	// the caller forever.
	sessionKillTimeout = 2 * time.Second
)

// startSession spawns spec.Argv under a fresh job object and returns
// a pipe Session. The process is created suspended
// (CREATE_SUSPENDED), assigned to the job before any user code runs,
// then resumed, so grandchildren cannot escape the job's limits.
//
// Policy validation belongs to the runner (validatePolicy); this
// constructor enforces mechanics only. spec.Opts.Resources.
// MaxOutputBytes bounds the replayable output ring when positive.
func startSession(ctx context.Context, spec sandbox.SessionSpec, cmd *exec.Cmd, confine bool, iso *netIsolation) (sandbox.Session, error) {
	if cmd == nil {
		return nil, errdefs.Validationf("windows: nil command for process session")
	}
	j, err := createJob(spec.Opts.Resources, spec.Opts.Timeout)
	if err != nil {
		return nil, err
	}
	abort := func(err error) (sandbox.Session, error) {
		if iso != nil {
			if cerr := iso.Close(); cerr != nil {
				telemetry.WarnErr(context.Background(),
					"windows: close net isolation after start failure", cerr)
			}
		}
		// KILL_ON_JOB_CLOSE terminates the child if it already ran.
		if cerr := j.close(); cerr != nil {
			telemetry.WarnErr(context.Background(),
				"windows: close job after start failure", cerr)
		}
		return nil, err
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return abort(classifyStartError(cmd.Path, err))
	}
	s := &winSession{
		id:         spec.ID,
		cmd:        cmd,
		job:        j,
		stdin:      stdin,
		out:        newOutputLog(spec.Opts.Resources.MaxOutputBytes),
		netIso:     iso,
		done:       make(chan struct{}),
		writeSlots: make(chan struct{}, sessionWriteConcurrency),
	}
	cmd.Stdout = sessionWriter{out: s.out, stream: sandbox.SessionStreamStdout}
	cmd.Stderr = sessionWriter{out: s.out, stream: sandbox.SessionStreamStderr}
	// os/exec copies child stdout/stderr into these writers, and
	// cmd.Wait drains them, so output ordering relative to process
	// exit is exact.
	cmd.SysProcAttr = &xwin.SysProcAttr{
		CreationFlags: xwin.CREATE_SUSPENDED | xwin.CREATE_NO_WINDOW,
	}
	if iso != nil {
		// The child runs under the AppContainer token: network is
		// kernel-isolated and writes are bounded by the package SID's
		// DACL grants, which replace the Low-IL confinement below.
		if err := enableCreateProcessAsUserPrivileges(); err != nil {
			return abort(err)
		}
		cmd.SysProcAttr.Token = syscall.Token(iso.token)
	} else if confine {
		// The child runs under a restricted, Low-integrity token:
		// reads everywhere, writes only where the mandatory label was
		// lowered. The handle stays valid through Start (the process
		// copies the token); closing it after is safe.
		if err := enableCreateProcessAsUserPrivileges(); err != nil {
			return abort(err)
		}
		token, err := restrictToken()
		if err != nil {
			return abort(err)
		}
		defer func() { _ = token.Close() }()
		cmd.SysProcAttr.Token = syscall.Token(token)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return abort(classifyStartError(cmd.Path, err))
	}
	s.proc = cmd.Process
	if err := j.assign(cmd.Process.Pid); err != nil {
		return abort(err)
	}
	if err := resumeProcess(cmd.Process.Pid); err != nil {
		return abort(err)
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

// startTTYSession spawns spec.Argv attached to a ConPTY pseudo console
// under a fresh job object. The child's stdout and stderr are merged
// into a single SessionStreamTTY stream, Resize maps to
// ResizePseudoConsole, and CloseInput is NotAvailable (the pseudo
// console is bidirectional). Signal stays NotAvailable: ConPTY has no
// portable Ctrl-C delivery, and writing the 0x03 byte is only reliable
// in cooked console modes.
//
// workDir and env must already be resolved by the caller (the runner
// applies EnvPolicy and workdir validation before calling this).
func startTTYSession(ctx context.Context, spec sandbox.SessionSpec, workDir string, env []string) (sandbox.Session, error) {
	rows, cols := uint32(spec.Rows), uint32(spec.Cols)
	if rows == 0 {
		rows = defaultTTYRows
	}
	if cols == 0 {
		cols = defaultTTYCols
	}

	j, err := createJob(spec.Opts.Resources, spec.Opts.Timeout)
	if err != nil {
		return nil, err
	}
	abort := func(err error) (sandbox.Session, error) {
		if cerr := j.close(); cerr != nil {
			telemetry.WarnErr(context.Background(),
				"windows: close job after tty start failure", cerr)
		}
		return nil, err
	}

	pt, err := newConPTY(rows, cols)
	if err != nil {
		return abort(err)
	}
	procH, pid, err := pt.spawn(spec.Argv, workDir, env)
	if err != nil {
		_ = pt.close()
		return abort(err)
	}
	s := &winSession{
		id:         spec.ID,
		job:        j,
		stdin:      pt.in,
		out:        newOutputLog(spec.Opts.Resources.MaxOutputBytes),
		pid:        pid,
		ttyPty:     pt,
		ttyProc:    procH,
		ttyCopy:    make(chan struct{}),
		done:       make(chan struct{}),
		writeSlots: make(chan struct{}, sessionWriteConcurrency),
	}
	if err := j.assign(pid); err != nil {
		// The child never ran; make sure it cannot linger suspended.
		_ = xwin.TerminateProcess(procH, 1)
		_ = xwin.CloseHandle(procH)
		_ = pt.close()
		return abort(err)
	}
	if err := resumeProcess(pid); err != nil {
		_ = xwin.TerminateProcess(procH, 1)
		_ = xwin.CloseHandle(procH)
		_ = pt.close()
		return abort(err)
	}

	go s.copyTTY()
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

// winSession is the concrete Windows session: the child's stdio
// pipes, the bounded replayable output log, and the owning job
// object. Pipe sessions use cmd/proc/stdin; TTY sessions use
// ttyPty/ttyProc/pid instead (cmd and proc stay nil).
type winSession struct {
	id         string
	cmd        *exec.Cmd // pipe sessions only (nil for TTY)
	job        *job
	stdin      io.WriteCloser
	proc       *os.Process // pipe sessions only (nil for TTY)
	out        *outputLog
	writeSlots chan struct{}
	netIso     *netIsolation // net-policy sessions (nil otherwise)

	pid     int           // TTY sessions: child pid (pipe: cmd.Process.Pid)
	ttyPty  *conpty       // TTY sessions: pseudo console (nil for pipe)
	ttyProc xwin.Handle   // TTY sessions: child process handle
	ttyCopy chan struct{} // TTY sessions: copy goroutine finished

	mu         sync.Mutex
	closed     bool
	timedOut   bool
	terminated bool
	exit       sandbox.SessionExit
	waitErr    error
	done       chan struct{}
}

func (s *winSession) ID() string { return s.id }

func (s *winSession) PID() int {
	if s.proc != nil {
		return s.proc.Pid
	}
	return s.pid
}

func (s *winSession) Read(ctx context.Context, afterSeq int64, maxBytes int) (sandbox.SessionOutput, error) {
	if s.isClosed() {
		return sandbox.SessionOutput{}, sandbox.ErrSessionClosed
	}
	return s.out.read(ctx, afterSeq, maxBytes)
}

func (s *winSession) Write(ctx context.Context, data []byte) error {
	if s.isClosed() {
		return sandbox.ErrSessionClosed
	}
	if s.stdin == nil {
		return sandbox.ErrSessionClosed
	}
	if len(data) == 0 {
		return nil
	}
	select {
	case s.writeSlots <- struct{}{}:
	case <-ctx.Done():
		return errdefs.FromContext(ctx.Err())
	case <-s.done:
		return sandbox.ErrSessionClosed
	}
	done := make(chan error, 1)
	go func() {
		defer func() { <-s.writeSlots }()
		_, err := io.Copy(s.stdin, bytes.NewReader(data))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			return errdefs.Internal(fmt.Errorf("windows: process write: %w", err))
		}
		return nil
	case <-ctx.Done():
		// The child is not draining; the write goroutine stays blocked
		// on the pipe until the session closes. Callers should not
		// retry the same bytes blindly.
		return ctx.Err()
	}
}

// CloseInput closes the session's stdin. Pipe sessions support it;
// TTY sessions cannot close their input (the pseudo console is
// bidirectional) and return NotAvailable, matching the shared
// Session contract.
func (s *winSession) CloseInput() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sandbox.ErrSessionClosed
	}
	if s.ttyPty != nil {
		return errdefs.NotAvailablef("windows: cannot close input on a TTY session")
	}
	if s.stdin == nil {
		return nil
	}
	err := s.stdin.Close()
	s.stdin = nil
	if errors.Is(err, os.ErrClosed) {
		// cmd.Wait (running in reap) closes the parent-side stdin
		// write end once the child exits. Racing that cleanup is a
		// successful no-op.
		return nil
	}
	return err
}

// Resize updates the pseudo console window size on TTY sessions;
// pipe sessions have no window and return NotAvailable.
func (s *winSession) Resize(_ context.Context, rows, cols int) error {
	if rows <= 0 || cols <= 0 {
		return errdefs.Validationf("windows: rows and cols must be positive")
	}
	if s.isClosed() {
		return sandbox.ErrSessionClosed
	}
	s.mu.Lock()
	pt := s.ttyPty
	s.mu.Unlock()
	if pt == nil {
		return errdefs.NotAvailablef("windows: Resize requires a TTY session")
	}
	return pt.resize(uint32(rows), uint32(cols))
}

// Signal is not available: Windows has no portable Ctrl-C delivery to
// a detached process tree, and ConPTY's input channel does not expose
// a reliable interrupt path. Use Terminate.
func (s *winSession) Signal(context.Context, sandbox.SessionSignal) error {
	return errdefs.NotAvailablef(
		"windows: signal delivery is not supported; use Terminate")
}

// Watch is not available: the phase-1 backend has no event streams.
func (s *winSession) Watch(context.Context) (sandbox.SessionWatcher, error) {
	return nil, errdefs.NotAvailablef("windows: event streams are not supported")
}

// Capabilities declares this session's actual surface: TTY sessions
// expose Resize + merged output; signal and event features stay off.
func (s *winSession) Capabilities() sandbox.SessionCapabilities {
	s.mu.Lock()
	tty := s.ttyPty != nil
	s.mu.Unlock()
	return sandbox.SessionCapabilities{TTY: tty}
}

// Terminate stops the whole job immediately (Windows has no SIGTERM).
// It is idempotent on an exited process and leaves the output log
// readable.
func (s *winSession) Terminate(ctx context.Context) error {
	if s.isClosed() {
		return sandbox.ErrSessionClosed
	}
	select {
	case <-s.done:
		return nil
	default:
	}
	s.mu.Lock()
	s.terminated = true
	s.mu.Unlock()
	if err := s.job.terminate(); err != nil {
		return err
	}
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Wait blocks until the process exits (or ctx is done) and returns
// the cached outcome; it is safe to call repeatedly and after Close.
func (s *winSession) Wait(ctx context.Context) (sandbox.SessionExit, error) {
	select {
	case <-s.done:
	case <-ctx.Done():
		return sandbox.SessionExit{}, ctx.Err()
	}
	s.mu.Lock()
	exit, err := s.exit, s.waitErr
	s.mu.Unlock()
	return exit, err
}

// Close terminates a still-running session, reaps it, and releases
// the output log and the job handle. Close is idempotent; the manager
// forgets the session so it no longer appears in List.
func (s *winSession) Close() error {
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
		_ = s.job.terminate()
		select {
		case <-s.done:
		case <-time.After(sessionKillTimeout):
			telemetry.Warn(context.Background(),
				"windows: job did not exit after TerminateJobObject on close",
				otellog.String("windows.session_id", s.id),
				otellog.Int("windows.pid", s.PID()))
		}
	}
	s.mu.Lock()
	stdin := s.stdin
	s.stdin = nil
	tty := s.ttyPty != nil
	s.mu.Unlock()
	if stdin != nil && !tty {
		if err := stdin.Close(); err != nil {
			telemetry.WarnErr(context.Background(), "windows: close session stdin failed", err,
				otellog.String("windows.session_id", s.id))
		}
	}
	if tty {
		// The TTY copy loop and waitTTY own the pseudo console pipes;
		// close is idempotent and also unblocks a stuck copy read.
		if err := s.ttyPty.close(); err != nil {
			telemetry.WarnErr(context.Background(), "windows: close tty failed", err,
				otellog.String("windows.session_id", s.id))
		}
	}
	if err := s.job.close(); err != nil {
		telemetry.WarnErr(context.Background(), "windows: close job failed", err,
			otellog.String("windows.session_id", s.id))
	}
	if s.netIso != nil {
		if err := s.netIso.Close(); err != nil {
			telemetry.WarnErr(context.Background(), "windows: close net isolation failed", err,
				otellog.String("windows.session_id", s.id))
		}
	}
	s.out.close()
	return nil
}

func (s *winSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// timeoutKill is ExecOptions.Timeout enforcement: the whole job is
// terminated, like Runner.Exec does on ctx deadline.
func (s *winSession) timeoutKill() {
	s.mu.Lock()
	s.timedOut = true
	s.mu.Unlock()
	_ = s.job.terminate()
}

// reap reaps the child, classifies the outcome, then finishes the
// output log. cmd.Wait also waits for the stdout/stderr copy
// goroutines (sessionWriter), so finish() after Wait never races
// buffered output. TTY sessions wait on the raw process handle and
// drain the pseudo console to EOF before finishing.
func (s *winSession) reap() {
	var waitErr error
	if s.ttyPty != nil {
		waitErr = s.waitTTY()
	} else {
		waitErr = s.cmd.Wait()
	}
	exit, err := s.classifyExit(waitErr)
	s.mu.Lock()
	s.exit = exit
	s.waitErr = err
	s.mu.Unlock()
	s.out.finish()
	close(s.done)
}

// waitTTY waits for the ConPTY child to exit, releases the console so
// the output channel reaches EOF, and drains the remaining output. It
// is the TTY counterpart of cmd.Wait.
func (s *winSession) waitTTY() error {
	if _, err := xwin.WaitForSingleObject(s.ttyProc, xwin.INFINITE); err != nil {
		return errdefs.Internal(fmt.Errorf("windows: wait tty process: %w", err))
	}
	var code uint32
	if err := xwin.GetExitCodeProcess(s.ttyProc, &code); err != nil {
		return errdefs.Internal(fmt.Errorf("windows: read tty exit code: %w", err))
	}
	_ = xwin.CloseHandle(s.ttyProc)
	s.ttyProc = 0

	// Releasing the console emits the final output frame and closes
	// the output channel; drain it before finishing the log.
	s.ttyPty.releaseConsole()
	select {
	case <-s.ttyCopy:
	case <-time.After(sessionKillTimeout):
		// Force a stuck copy read out of the pipe.
		_ = s.ttyPty.close()
		<-s.ttyCopy
	}
	_ = s.ttyPty.close()

	if code == 0 {
		return nil
	}
	return &ttyExitError{code: int(code)}
}

// copyTTY drains the pseudo console output into the merged TTY
// stream until EOF or the pipe is closed.
func (s *winSession) copyTTY() {
	defer close(s.ttyCopy)
	buf := make([]byte, 32*1024)
	for {
		n, err := s.ttyPty.out.Read(buf)
		if n > 0 {
			s.out.append(sandbox.SessionStreamTTY, buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (s *winSession) classifyExit(waitErr error) (sandbox.SessionExit, error) {
	s.mu.Lock()
	timedOut := s.timedOut
	terminated := s.terminated
	s.mu.Unlock()
	// A cap-kill completion message can land a moment after the
	// process exits; give the notifier a short grace window so a
	// kernel-enforced limit is classified as BudgetExceeded rather
	// than a plain exit.
	if s.job.notify != nil {
		deadline := time.Now().Add(100 * time.Millisecond)
		for s.job.budgetCap() == "" && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if cap := s.job.budgetCap(); cap != "" {
			return sandbox.SessionExit{Code: -1, Reason: sandbox.SessionBudgetExceeded},
				errdefs.BudgetExceededf(
					"windows: %s resource cap exceeded while running process", cap)
		}
	}
	if timedOut {
		return sandbox.SessionExit{Code: -1, Reason: sandbox.SessionTimedOut},
			errdefs.FromContext(fmt.Errorf(
				"windows: process exceeded its Timeout: %w", context.DeadlineExceeded))
	}
	if waitErr == nil {
		return sandbox.SessionExit{Code: 0, Reason: sandbox.SessionExited}, nil
	}
	if exitErr, ok := waitErr.(interface{ ExitCode() int }); ok {
		if terminated {
			return sandbox.SessionExit{Code: -1, Reason: sandbox.SessionTerminated}, nil
		}
		return sandbox.SessionExit{Code: exitErr.ExitCode(), Reason: sandbox.SessionExited}, nil
	}
	return sandbox.SessionExit{Code: -1, Reason: sandbox.SessionExited},
		errdefs.Internal(fmt.Errorf("windows: process wait: %w", waitErr))
}

// sessionWriter is exec.Cmd's stdout/stderr sink for pipe sessions.
// cmd.Wait waits for these writers, so output ordering relative to
// process exit is exact.
type sessionWriter struct {
	out    *outputLog
	stream sandbox.SessionStream
}

func (w sessionWriter) Write(p []byte) (int, error) {
	w.out.append(w.stream, p)
	return len(p), nil
}

// outputLog is the append-only, bounded, replayable output buffer
// (a simplified port of core/sandbox's unix outputLog without event
// subscribers). Seq is a byte cursor: each chunk records the sequence
// of its first byte and the log advances by len(data). When the ring
// budget is exceeded, oldest whole chunks are dropped and Read
// reports ErrSequenceGap for cursors below the retained range.
type outputLog struct {
	mu      sync.Mutex
	wake    chan struct{}
	chunks  []outputChunk
	total   int64
	nextSeq int64
	max     int64
	eof     bool
	closed  bool
}

type outputChunk struct {
	seq    int64
	stream sandbox.SessionStream
	data   []byte
}

func newOutputLog(maxBytes int64) *outputLog {
	return &outputLog{wake: make(chan struct{}), max: maxBytes}
}

func (l *outputLog) append(stream sandbox.SessionStream, data []byte) {
	if len(data) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.chunks = append(l.chunks, outputChunk{
		seq:    l.nextSeq,
		stream: stream,
		data:   append([]byte(nil), data...),
	})
	l.total += int64(len(data))
	l.nextSeq += int64(len(data))
	l.trimLocked()
	l.wakeReadersLocked()
}

// finish marks the stream complete and wakes blocked readers so a
// Read at the end of output returns EOF.
func (l *outputLog) finish() {
	l.mu.Lock()
	l.eof = true
	l.wakeReadersLocked()
	l.mu.Unlock()
}

// close marks the log closed: reads fail with ErrSessionClosed.
func (l *outputLog) close() {
	l.mu.Lock()
	l.closed = true
	l.wakeReadersLocked()
	l.mu.Unlock()
}

// read returns output at/after afterSeq, at most maxBytes, blocking
// until data, EOF, or ctx cancellation.
func (l *outputLog) read(ctx context.Context, afterSeq int64, maxBytes int) (sandbox.SessionOutput, error) {
	if maxBytes <= 0 {
		return sandbox.SessionOutput{}, errdefs.Validationf(
			"windows: Read maxBytes must be positive")
	}
	l.mu.Lock()
	for {
		if l.closed {
			l.mu.Unlock()
			return sandbox.SessionOutput{}, sandbox.ErrSessionClosed
		}
		if retained := l.retainedSeqLocked(); retained > afterSeq {
			l.mu.Unlock()
			return sandbox.SessionOutput{}, fmt.Errorf("%w: afterSeq %d, retained from %d",
				sandbox.ErrSequenceGap, afterSeq, retained)
		}
		if l.nextSeq < afterSeq {
			l.mu.Unlock()
			return sandbox.SessionOutput{}, errdefs.Validationf(
				"windows: afterSeq %d is beyond buffered output (next=%d)",
				afterSeq, l.nextSeq)
		}
		if out, ok := l.collectLocked(afterSeq, maxBytes); ok {
			l.mu.Unlock()
			return out, nil
		}
		if l.eof {
			l.mu.Unlock()
			return sandbox.SessionOutput{NextSeq: afterSeq, EOF: true}, nil
		}
		wake := l.wake
		l.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return sandbox.SessionOutput{}, ctx.Err()
		}
		l.mu.Lock()
	}
}

func (l *outputLog) collectLocked(afterSeq int64, maxBytes int) (sandbox.SessionOutput, bool) {
	remaining := int64(maxBytes)
	next := afterSeq
	var chunks []sandbox.OutputChunk
	for _, ch := range l.chunks {
		if remaining <= 0 {
			break
		}
		end := ch.seq + int64(len(ch.data))
		if next >= end {
			continue
		}
		start := next - ch.seq
		n := int64(len(ch.data)) - start
		if n > remaining {
			n = remaining
		}
		chunks = append(chunks, sandbox.OutputChunk{
			Seq:    next,
			Stream: ch.stream,
			Data:   append([]byte(nil), ch.data[start:start+n]...),
		})
		next += n
		remaining -= n
	}
	if len(chunks) == 0 {
		return sandbox.SessionOutput{}, false
	}
	return sandbox.SessionOutput{
		NextSeq: next,
		Chunks:  chunks,
		EOF:     l.eof && next == l.nextSeq,
	}, true
}

func (l *outputLog) retainedSeqLocked() int64 {
	if len(l.chunks) == 0 {
		return l.nextSeq
	}
	return l.chunks[0].seq
}

func (l *outputLog) trimLocked() {
	if l.max <= 0 {
		return
	}
	// Never drop the only chunk: a single chunk larger than the
	// budget is bounded by the pipe copy buffer and stays replayable.
	for len(l.chunks) > 1 && l.total > l.max {
		l.total -= int64(len(l.chunks[0].data))
		l.chunks = l.chunks[1:]
	}
}

// wakeReadersLocked notifies blocked Reads that the log changed. The
// closed channel is immediately replaced so future waits get a fresh
// signal channel.
func (l *outputLog) wakeReadersLocked() {
	close(l.wake)
	l.wake = make(chan struct{})
}

func classifyStartError(path string, err error) error {
	if errors.Is(err, xwin.ERROR_PRIVILEGE_NOT_HELD) {
		return errdefs.NotAvailablef(
			"windows: start %s: write confinement needs the host to hold SE_INCREASE_QUOTA_NAME (run elevated or as a service): %v",
			path, err)
	}
	return errdefs.Internal(fmt.Errorf("windows: start %s: %w", path, err))
}
