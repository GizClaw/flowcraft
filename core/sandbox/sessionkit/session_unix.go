//go:build unix

package sessionkit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"github.com/creack/pty"
	"github.com/rs/xid"
	"go.opentelemetry.io/otel/attribute"
)

const (
	defaultSessionRows = 24
	defaultSessionCols = 80
	sessionCopyChunk   = 32 * 1024
	// watcherQueueSize bounds each event subscriber's live queue.
	// Overflow surfaces as SessionEventLag followed by the watcher
	// closing; consumers recover with Read(afterSeq).
	watcherQueueSize = 256
	// sessionTerminateGrace is how long Terminate waits after SIGTERM
	// before escalating to SIGKILL, so TUI/REPL children get a chance
	// to restore the terminal and flush state.
	sessionTerminateGrace   = 2 * time.Second
	sessionWriteConcurrency = 4
	// sessionKillTimeout bounds how long Close waits for the process
	// group to die after SIGKILL. A child that ignores SIGKILL (or a
	// failed delivery) must not hang the caller forever.
	sessionKillTimeout = 2 * time.Second
)

// startWithCleanSignalMask runs one process spawn from a thread whose
// signal mask is empty.
//
// A child inherits the forking thread's mask, and a mask survives exec
// while dispositions do not. Go reuses threads that entered the runtime
// from C, and those keep the mask their thread arrived with, so a fork on
// one of them can hand the session a child that starts with SIGINT
// blocked: kill(2) to its group succeeds, the signal stays pending, and
// the process runs to its own end - an interrupt that silently does
// nothing. Pinning the goroutine makes the cleared mask the one the fork
// sees; the caller's own mask goes back before this returns. Locking is
// counted, so a caller that pinned itself keeps its pin.
//
// The guarantee covers every child the sandbox spawns that can receive
// [Session.Signal]: StartSession's two paths (pty and pipes) and anything
// else that spawns through this seam. A new spawn site that hands its
// process to Session.Signal has to come through here too.
//
// Only the spawn runs with the cleared mask - the fork and the exec - so
// the window the calling thread spends unblocked is that spawn and
// nothing else. Reading or writing the mask can fail, and a platform may
// expose no mask API at all (sigmask_other_unix.go); a spawn must not fail
// over that, so the process starts either way and only the mask guarantee
// is lost. Linux and Darwin implement it.
func startWithCleanSignalMask(start func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	previous, err := replaceThreadSignalMask(emptySigset())
	if err != nil {
		return start()
	}
	defer func() { _, _ = replaceThreadSignalMask(previous) }()
	return start()
}

// StartSession launches an already-configured *exec.Cmd as a Session.
// cmd.Dir / cmd.Env must already be resolved by the caller; StartSession
// owns the stdio plumbing only:
//
//   - tty=true: a pty becomes the child's controlling terminal; stdout
//     and stderr are merged into SessionStreamTTY.
//   - tty=false: stdin is piped only when spec.Stdin declares an input
//     side; stdout/stderr are tagged streams. Without that declaration
//     cmd.Stdin stays nil, so os/exec gives the child /dev/null and a
//     program that tests fstat(0) reads its operands instead.
//
// Policy validation belongs to the backend (see ValidateExecPolicy);
// this constructor enforces mechanics only. spec.Opts.Resources.
// MaxOutputBytes bounds the replayable output ring when positive;
// non-positive keeps all output (callers that want the default cap
// must apply it, as the built-in runners do).
//
// StartSession is the shared seam the built-in runners use so
// seatbelt/bwrap/local all get identical seq, resize, and
// termination semantics.
//
// The returned Session always carries a stable ID: spec.ID when set,
// otherwise a manager-generated one. Built-in registries resolve the
// ID before spawning, so Runner.List / Terminate and the
// handle's ID() always agree.
func StartSession(ctx context.Context, spec sandbox.SessionSpec, cmd *exec.Cmd) (sandbox.Session, error) {
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

	s := &localSession{
		id:         id,
		cmd:        cmd,
		out:        newOutputLog(spec.Opts.Resources.MaxOutputBytes),
		input:      spec.Stdin || spec.TTY,
		done:       make(chan struct{}),
		writeSlots: make(chan struct{}, sessionWriteConcurrency),
	}

	if spec.TTY {
		var ptmx *os.File
		err := startWithCleanSignalMask(func() error {
			var startErr error
			ptmx, startErr = pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
			return startErr
		})
		if err != nil {
			return nil, classifyStartError(cmd.Path, err)
		}
		s.ptmx = ptmx
		s.stdin = ptmx
		s.pgid = cmd.Process.Pid
		s.copiers.Add(1)
		go func() {
			defer s.copiers.Done()
			s.copyLoop(ptmx)
		}()
	} else {
		var stdin io.WriteCloser
		if s.input {
			var err error
			stdin, err = cmd.StdinPipe()
			if err != nil {
				return nil, classifyStartError(cmd.Path, err)
			}
		}
		cmd.Stdout = sessionWriter{out: s.out, stream: sandbox.SessionStreamStdout}
		cmd.Stderr = sessionWriter{out: s.out, stream: sandbox.SessionStreamStderr}
		// Unlike Exec, the session owns cancellation: no CommandContext
		// Cancel hook (which would reject a plain exec.Command), and no
		// ctx kill — the session lives until Terminate/Close.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := startWithCleanSignalMask(cmd.Start); err != nil {
			if stdin != nil {
				if cerr := stdin.Close(); cerr != nil {
					telemetry.WarnErr(ctx, "sandbox: close stdin pipe after start failure", cerr,
						attribute.String("sandbox.session_id", s.id))
				}
			}
			return nil, classifyStartError(cmd.Path, err)
		}
		s.stdin = stdin
		s.pgid = cmd.Process.Pid
	}
	s.proc = cmd.Process

	if spec.Opts.Resources.MemoryBytes > 0 || spec.Opts.Resources.CPUMillicores > 0 {
		// The Start ctx only bounds the spawn; the session outlives it.
		// WithoutCancel keeps the watcher's telemetry rooted in the
		// originating trace without making a canceled Start kill the
		// session.
		s.watcher = StartGroupCapsWatcher(context.WithoutCancel(ctx), s.pgid, spec.Opts.Resources, spec.Opts.Timeout)
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

// localSession is the concrete unix backend session: it owns the
// child's stdio (pipes or pty), the bounded replayable output log,
// and the resource-cap watcher.
type localSession struct {
	id         string
	cmd        *exec.Cmd
	ptmx       *os.File
	stdin      io.WriteCloser
	input      bool // an input side exists: Write has somewhere to go
	proc       *os.Process
	pgid       int
	out        *outputLog
	watcher    *GroupCapsWatcher
	copiers    sync.WaitGroup
	writeSlots chan struct{}

	mu         sync.Mutex
	closed     bool
	timedOut   bool
	terminated bool
	exit       sandbox.SessionExit
	waitErr    error
	done       chan struct{}
}

func (s *localSession) ID() string { return s.id }

func (s *localSession) PID() int {
	if s.proc == nil {
		return 0
	}
	return s.proc.Pid
}

func (s *localSession) Read(ctx context.Context, afterSeq int64, maxBytes int) (sandbox.SessionOutput, error) {
	if s.isClosed() {
		return sandbox.SessionOutput{}, sandbox.ErrSessionClosed
	}
	return s.out.read(ctx, afterSeq, maxBytes)
}

func (s *localSession) Write(ctx context.Context, data []byte) error {
	if s.isClosed() {
		return sandbox.ErrSessionClosed
	}
	// Snapshot stdin under the lock: CloseInput / Close nil it out
	// concurrently, so the writer goroutine below must never read the
	// field itself (that would be a data race and could panic on a nil
	// writer). input is set once at spawn and needs no lock.
	s.mu.Lock()
	stdin := s.stdin
	s.mu.Unlock()
	if stdin == nil {
		if !s.input {
			return errdefs.NotAvailablef(
				"sandbox: session started without stdin; set SessionSpec.Stdin to stream input")
		}
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
		_, err := io.Copy(stdin, bytes.NewReader(data))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			// A child that closed its input — or exited — before the bytes
			// arrived is not a failure of this write: probing fd 0 and
			// leaving, or exiting on the first line, are ordinary. The
			// session's terminal status is where that shows up.
			if isClosedInput(err) {
				return nil
			}
			return errdefs.Internal(fmt.Errorf("sandbox: process write: %w", err))
		}
		return nil
	case <-ctx.Done():
		// The child is not draining; the write goroutine stays blocked
		// on the fd until the session closes. Callers should not retry
		// the same bytes blindly.
		return ctx.Err()
	}
}

// CloseInput closes the session's stdin. TTY sessions cannot close
// their input (the pty is bidirectional) and return NotAvailable; a
// pipe session started without an input side is a no-op success, so
// Exec can call it unconditionally.
func (s *localSession) CloseInput() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sandbox.ErrSessionClosed
	}
	if s.ptmx != nil {
		return errdefs.NotAvailablef(
			"sandbox: cannot close input on a TTY session")
	}
	if s.stdin == nil {
		return nil
	}
	err := s.stdin.Close()
	s.stdin = nil
	if errors.Is(err, os.ErrClosed) {
		// cmd.Wait (running in reap) closes the parent-side stdin write
		// end once the child exits. Racing that cleanup is a successful
		// no-op: the input is already closed, which is the desired state.
		return nil
	}
	return err
}

func (s *localSession) Resize(_ context.Context, rows, cols int) error {
	if rows <= 0 || cols <= 0 {
		return errdefs.Validationf("sandbox: rows and cols must be positive")
	}
	if s.isClosed() {
		return sandbox.ErrSessionClosed
	}
	s.mu.Lock()
	ptmx := s.ptmx
	if ptmx == nil {
		s.mu.Unlock()
		return errdefs.NotAvailablef("sandbox: Resize requires a TTY session")
	}
	// Setsize uses Fd(), which is not safe against a concurrent Close
	// (reap / Close nil the field under the same lock, then close the
	// fd outside it). Holding the lock across the ioctl serializes the
	// two paths.
	err := pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	s.mu.Unlock()
	return err
}

func (s *localSession) Terminate(ctx context.Context) error {
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
	return s.signal(ctx, false)
}

// Signal implements Session: interrupt means Ctrl-C semantics — a
// VINTR byte on TTY sessions (the terminal driver signals the
// foreground process group), SIGINT to the whole group on pipe
// sessions. The process may catch the signal and continue; the session
// stays usable.
func (s *localSession) Signal(_ context.Context, sig sandbox.SessionSignal) error {
	if sig != sandbox.SessionSignalInterrupt {
		return errdefs.NotAvailablef("sandbox: signal %v not supported", sig)
	}
	if s.isClosed() {
		return sandbox.ErrSessionClosed
	}
	select {
	case <-s.done:
		return sandbox.ErrSessionClosed
	default:
	}
	s.mu.Lock()
	ptmx := s.ptmx
	s.mu.Unlock()
	if ptmx != nil {
		// VINTR through the pty master is a no-op when the child put
		// the terminal in raw mode (ISIG off) — authentic Ctrl-C
		// behaviour, documented on Session.
		if _, err := ptmx.Write([]byte{0x03}); err != nil {
			return errdefs.Internal(fmt.Errorf("sandbox: signal write to pty: %w", err))
		}
		return nil
	}
	if err := syscall.Kill(-s.pgid, syscall.SIGINT); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return sandbox.ErrSessionClosed
		}
		return errdefs.Internal(fmt.Errorf("sandbox: signal process group: %w", err))
	}
	return nil
}

// Watch implements Session: it subscribes an independent
// bounded queue that replays the retained output before delivering
// live events. The channel closes on ctx cancellation, watcher.Close,
// or right after the process's Closed event.
func (s *localSession) Watch(ctx context.Context) (sandbox.SessionWatcher, error) {
	if s.isClosed() {
		return nil, sandbox.ErrSessionClosed
	}
	s.out.mu.Lock()
	if s.out.closed {
		s.out.mu.Unlock()
		return nil, sandbox.ErrSessionClosed
	}
	w := s.out.subscribe()
	if len(s.out.chunks) > watcherQueueSize {
		// Replay alone would overflow the queue; skip the partial
		// replay and deliver one Lag from the retained start instead.
		start := s.out.retainedSeqLocked()
		w.ch <- sandbox.SessionEvent{Seq: start, Type: sandbox.SessionEventLag}
		for i, sub := range s.out.subscribers {
			if sub == w {
				s.out.subscribers = append(s.out.subscribers[:i], s.out.subscribers[i+1:]...)
				break
			}
		}
		w.once.Do(func() { close(w.ch) })
		s.out.mu.Unlock()
		return w, nil
	}
	for _, chunk := range s.out.chunks {
		w.ch <- sandbox.SessionEvent{
			Seq:    chunk.seq,
			Stream: chunk.stream,
			Data:   chunk.data,
			Type:   sandbox.SessionEventOutput,
		}
	}
	if s.out.eof {
		w.ch <- sandbox.SessionEvent{Seq: s.out.nextSeq, Type: sandbox.SessionEventExited, Exit: &s.out.exit}
	}
	s.out.mu.Unlock()
	go w.run(ctx)
	return w, nil
}

// Capabilities declares this session's actual surface. On unix the
// local backend supports Signal and Watch on every session; TTY is
// true only for pty sessions (which also makes Resize available and
// CloseInput NotAvailable). Stdin reports whether Write has somewhere
// to go: a pipe session that declared input, or any pty session.
func (s *localSession) Capabilities() sandbox.SessionCapabilities {
	s.mu.Lock()
	tty := s.ptmx != nil
	s.mu.Unlock()
	return sandbox.SessionCapabilities{TTY: tty, Stdin: s.input, Signal: true, Events: true}
}

func (s *localSession) Wait(ctx context.Context) (sandbox.SessionExit, error) {
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

func (s *localSession) Close() error {
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
		s.killGroup(syscall.SIGKILL, "sandbox: SIGKILL process group on close failed")
		select {
		case <-s.done:
		case <-time.After(sessionKillTimeout):
			telemetry.Warn(context.Background(),
				"sandbox: process group did not exit after SIGKILL on close",
				attribute.String("sandbox.session_id", s.id),
				attribute.Int("sandbox.pgid", s.pgid))
		}
	}
	s.mu.Lock()
	ptmx := s.ptmx
	s.ptmx = nil
	s.mu.Unlock()
	if ptmx != nil {
		if err := ptmx.Close(); err != nil {
			telemetry.WarnErr(context.Background(), "sandbox: close pty master failed", err,
				attribute.String("sandbox.session_id", s.id))
		}
	}
	s.mu.Lock()
	stdin := s.stdin
	s.stdin = nil
	s.mu.Unlock()
	if stdin != nil && stdin != ptmx {
		if err := stdin.Close(); err != nil {
			telemetry.WarnErr(context.Background(), "sandbox: close session stdin failed", err,
				attribute.String("sandbox.session_id", s.id))
		}
	}
	s.out.close()
	return nil
}

func (s *localSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// timeoutKill is the ExecOptions.Timeout enforcement: the session is
// killed with SIGKILL, like Runner.Exec does on ctx deadline.
func (s *localSession) timeoutKill() {
	s.mu.Lock()
	s.timedOut = true
	s.mu.Unlock()
	_ = s.signal(context.Background(), true)
}

// signal stops the session. force uses SIGKILL directly (timeout /
// close path); otherwise SIGTERM first with a short grace period
// bounded by ctx.
func (s *localSession) signal(ctx context.Context, force bool) error {
	if force {
		s.killGroup(syscall.SIGKILL, "sandbox: SIGKILL process group failed")
		return nil
	}
	s.killGroup(syscall.SIGTERM, "sandbox: SIGTERM process group failed")

	grace := sessionTerminateGrace
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < grace {
			grace = remaining
		}
	}
	if grace <= 0 {
		s.killGroup(syscall.SIGKILL, "sandbox: SIGKILL process group after grace failed")
		return ctx.Err()
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-s.done:
		return nil
	case <-timer.C:
		s.killGroup(syscall.SIGKILL, "sandbox: SIGKILL process group on grace timeout failed")
		return nil
	case <-ctx.Done():
		s.killGroup(syscall.SIGKILL, "sandbox: SIGKILL process group on cancel failed")
		return ctx.Err()
	}
}

// killGroup delivers sig to the process group and leaves a failed
// delivery visible to telemetry. Kill failures during teardown are
// best-effort by contract (the child may already be gone), so they are
// never propagated to callers.
func (s *localSession) killGroup(sig syscall.Signal, msg string) {
	if err := syscall.Kill(-s.pgid, sig); err != nil {
		telemetry.WarnErr(context.Background(), msg, err,
			attribute.String("sandbox.session_id", s.id),
			attribute.Int("sandbox.pgid", s.pgid))
	}
}

// reap reaps the child, classifies the outcome, then finishes the
// output log. cmd.Wait also waits for the stdout/stderr copy goroutines
// (sessionWriter), so finish() after Wait never races buffered output.
func (s *localSession) reap() {
	waitErr := s.cmd.Wait()
	if s.watcher != nil {
		s.watcher.Stop()
	}
	s.mu.Lock()
	ptmx := s.ptmx
	s.ptmx = nil
	s.mu.Unlock()
	if ptmx != nil {
		if err := ptmx.Close(); err != nil {
			telemetry.WarnErr(context.Background(), "sandbox: close pty after reap failed", err,
				attribute.String("sandbox.session_id", s.id))
		}
	}
	// The pty copier may still hold bytes read before the child's last
	// write flushed; wait for it so EOF is never reported early.
	s.copiers.Wait()

	exit, err := s.classifyExit(waitErr)
	s.mu.Lock()
	s.exit = exit
	s.waitErr = err
	s.mu.Unlock()
	s.out.finish(s.exit)
	close(s.done)
}

func (s *localSession) classifyExit(waitErr error) (sandbox.SessionExit, error) {
	if s.watcher != nil {
		// Order matters: a watcher that gave up on sampling killed the
		// group without proving any budget was exceeded, so reporting
		// BudgetExceeded there would be a false accusation.
		if sampleErr := s.watcher.Unenforceable(); sampleErr != nil {
			return sandbox.SessionExit{Code: -1, Reason: sandbox.SessionUnenforceable},
				errdefs.NotAvailablef(
					"sandbox: resource caps became unenforceable while running process: %w", sampleErr)
		}
		if cap := s.watcher.Exceeded(); cap != "" {
			return sandbox.SessionExit{Code: -1, Reason: sandbox.SessionBudgetExceeded},
				errdefs.BudgetExceededf(
					"sandbox: %s resource cap exceeded while running process", cap)
		}
	}

	s.mu.Lock()
	timedOut := s.timedOut
	terminated := s.terminated
	s.mu.Unlock()
	if timedOut {
		return sandbox.SessionExit{Code: -1, Reason: sandbox.SessionTimedOut},
			errdefs.FromContext(fmt.Errorf("sandbox: process exceeded its Timeout: %w", context.DeadlineExceeded))
	}
	if waitErr == nil {
		return sandbox.SessionExit{Code: 0, Reason: sandbox.SessionExited}, nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			if terminated {
				return sandbox.SessionExit{Code: -1, Signal: int(ws.Signal()), Reason: sandbox.SessionTerminated}, nil
			}
			return sandbox.SessionExit{Code: -1, Signal: int(ws.Signal()), Reason: sandbox.SessionSignaled}, nil
		}
		return sandbox.SessionExit{Code: exitErr.ExitCode(), Reason: sandbox.SessionExited}, nil
	}
	return sandbox.SessionExit{Code: -1, Reason: sandbox.SessionExited},
		errdefs.Internal(fmt.Errorf("sandbox: process wait: %w", waitErr))
}

func (s *localSession) copyLoop(r io.Reader) {
	buf := make([]byte, sessionCopyChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.out.append(sandbox.SessionStreamTTY, buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// sessionWriter is exec.Cmd's stdout/stderr sink for pipe sessions.
// cmd.Wait waits for these writers, so output ordering relative to
// process exit is exact.
// isClosedInput reports whether a write failed because the far end is no
// longer reading: the child closed its stdin, or the session's own descriptor
// is gone. Both mean "stop writing", not "the write broke".
func isClosedInput(err error) bool {
	return errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.EBADF) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, os.ErrClosed)
}
