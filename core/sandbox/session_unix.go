//go:build unix

package sandbox

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
	"github.com/GizClaw/flowcraft/core/telemetry"

	"github.com/creack/pty"
	"github.com/rs/xid"
	otellog "go.opentelemetry.io/otel/log"
)

const (
	defaultSessionRows = 24
	defaultSessionCols = 80
	sessionCopyChunk   = 32 * 1024
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

// StartSession launches an already-configured *exec.Cmd as a Session.
// cmd.Dir / cmd.Env must already be resolved by the caller; StartSession
// owns the stdio plumbing only:
//
//   - tty=true: a pty becomes the child's controlling terminal; stdout
//     and stderr are merged into SessionStreamTTY.
//   - tty=false: stdin is piped and stdout/stderr are tagged streams.
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
func StartSession(ctx context.Context, spec SessionSpec, cmd *exec.Cmd) (Session, error) {
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
		done:       make(chan struct{}),
		writeSlots: make(chan struct{}, sessionWriteConcurrency),
	}

	if spec.TTY {
		ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
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
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, classifyStartError(cmd.Path, err)
		}
		cmd.Stdout = sessionWriter{out: s.out, stream: SessionStreamStdout}
		cmd.Stderr = sessionWriter{out: s.out, stream: SessionStreamStderr}
		// Unlike Exec, the session owns cancellation: no CommandContext
		// Cancel hook (which would reject a plain exec.Command), and no
		// ctx kill — the session lives until Terminate/Close.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			if cerr := stdin.Close(); cerr != nil {
				telemetry.WarnErr(ctx, "sandbox: close stdin pipe after start failure", cerr,
					otellog.String("sandbox.session_id", s.id))
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
	exit       SessionExit
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

func (s *localSession) Read(ctx context.Context, afterSeq int64, maxBytes int) (SessionOutput, error) {
	if s.isClosed() {
		return SessionOutput{}, ErrSessionClosed
	}
	return s.out.read(ctx, afterSeq, maxBytes)
}

func (s *localSession) Write(ctx context.Context, data []byte) error {
	if s.isClosed() {
		return ErrSessionClosed
	}
	if s.stdin == nil {
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
	done := make(chan error, 1)
	go func() {
		defer func() { <-s.writeSlots }()
		_, err := io.Copy(s.stdin, bytes.NewReader(data))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
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
// their input (the pty is bidirectional) and return NotAvailable.
func (s *localSession) CloseInput() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSessionClosed
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
		return ErrSessionClosed
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
	return s.signal(ctx, false)
}

// Signal implements Session: interrupt means Ctrl-C semantics — a
// VINTR byte on TTY sessions (the terminal driver signals the
// foreground process group), SIGINT to the whole group on pipe
// sessions. The process may catch the signal and continue; the session
// stays usable.
func (s *localSession) Signal(_ context.Context, sig SessionSignal) error {
	if sig != SessionSignalInterrupt {
		return errdefs.NotAvailablef("sandbox: signal %v not supported", sig)
	}
	if s.isClosed() {
		return ErrSessionClosed
	}
	select {
	case <-s.done:
		return ErrSessionClosed
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
			return ErrSessionClosed
		}
		return errdefs.Internal(fmt.Errorf("sandbox: signal process group: %w", err))
	}
	return nil
}

// Watch implements Session: it subscribes an independent
// bounded queue that replays the retained output before delivering
// live events. The channel closes on ctx cancellation, watcher.Close,
// or right after the process's Closed event.
func (s *localSession) Watch(ctx context.Context) (SessionWatcher, error) {
	if s.isClosed() {
		return nil, ErrSessionClosed
	}
	return watchOutputLog(ctx, s.out)
}

// Capabilities declares this session's actual surface. On unix the
// local backend supports Signal and Watch on every session; TTY is
// true only for pty sessions (which also makes Resize available and
// CloseInput NotAvailable).
func (s *localSession) Capabilities() SessionCapabilities {
	s.mu.Lock()
	tty := s.ptmx != nil
	s.mu.Unlock()
	return SessionCapabilities{TTY: tty, Signal: true, Events: true}
}

func (s *localSession) Wait(ctx context.Context) (SessionExit, error) {
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
				otellog.String("sandbox.session_id", s.id),
				otellog.Int("sandbox.pgid", s.pgid))
		}
	}
	s.mu.Lock()
	ptmx := s.ptmx
	s.ptmx = nil
	s.mu.Unlock()
	if ptmx != nil {
		if err := ptmx.Close(); err != nil {
			telemetry.WarnErr(context.Background(), "sandbox: close pty master failed", err,
				otellog.String("sandbox.session_id", s.id))
		}
	}
	s.mu.Lock()
	stdin := s.stdin
	s.stdin = nil
	s.mu.Unlock()
	if stdin != nil && stdin != ptmx {
		if err := stdin.Close(); err != nil {
			telemetry.WarnErr(context.Background(), "sandbox: close session stdin failed", err,
				otellog.String("sandbox.session_id", s.id))
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
			otellog.String("sandbox.session_id", s.id),
			otellog.Int("sandbox.pgid", s.pgid))
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
				otellog.String("sandbox.session_id", s.id))
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

func (s *localSession) classifyExit(waitErr error) (SessionExit, error) {
	if s.watcher != nil {
		// Order matters: a watcher that gave up on sampling killed the
		// group without proving any budget was exceeded, so reporting
		// BudgetExceeded there would be a false accusation.
		if sampleErr := s.watcher.Unenforceable(); sampleErr != nil {
			return SessionExit{Code: -1, Reason: SessionUnenforceable},
				errdefs.NotAvailablef(
					"sandbox: resource caps became unenforceable while running process: %v", sampleErr)
		}
		if cap := s.watcher.Exceeded(); cap != "" {
			return SessionExit{Code: -1, Reason: SessionBudgetExceeded},
				errdefs.BudgetExceededf(
					"sandbox: %s resource cap exceeded while running process", cap)
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
	if waitErr == nil {
		return SessionExit{Code: 0, Reason: SessionExited}, nil
	}
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			if terminated {
				return SessionExit{Code: -1, Signal: int(ws.Signal()), Reason: SessionTerminated}, nil
			}
			return SessionExit{Code: -1, Signal: int(ws.Signal()), Reason: SessionSignaled}, nil
		}
		return SessionExit{Code: exitErr.ExitCode(), Reason: SessionExited}, nil
	}
	return SessionExit{Code: -1, Reason: SessionExited},
		errdefs.Internal(fmt.Errorf("sandbox: process wait: %w", waitErr))
}

func (s *localSession) copyLoop(r io.Reader) {
	buf := make([]byte, sessionCopyChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.out.append(SessionStreamTTY, buf[:n])
		}
		if err != nil {
			return
		}
	}
}
