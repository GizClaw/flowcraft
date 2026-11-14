//go:build unix

package sessionkit

import (
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
	"github.com/rs/xid"
	"go.opentelemetry.io/otel/attribute"

	"github.com/creack/pty"
)

const (
	defaultSessionRows = 24
	defaultSessionCols = 80
	sessionCopyChunk   = 32 * 1024
)

// startWithCleanSignalMask runs one process spawn from a thread whose signal mask
// is empty: a child inherits the forking thread's mask, and a mask survives exec.
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

// StartSession starts one session over the unix spawn path: a pty when the spec
// asks for a TTY, pipes otherwise, with the child in its own process group so the
// whole tree can be signalled and killed.
//
// It fills in the port and hands the lifecycle to the shared state machine: this
// file is the platform half, not a second session.
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

	s := &session{
		id:         id,
		out:        newOutputLog(spec.Opts.Resources.MaxOutputBytes),
		input:      spec.Stdin || spec.TTY,
		done:       make(chan struct{}),
		writeSlots: make(chan struct{}, sessionWriteConcurrency),
	}

	var (
		ptmx    *os.File
		ptyMu   sync.Mutex
		copiers sync.WaitGroup
	)
	tty := spec.TTY
	if tty {
		err := startWithCleanSignalMask(func() error {
			var startErr error
			ptmx, startErr = pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
			return startErr
		})
		if err != nil {
			return nil, classifyStartError(cmd.Path, err)
		}
		s.stdin = ptmx
		copiers.Add(1)
		go func() {
			defer copiers.Done()
			copyLoop(s.out, ptmx)
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
		// Unlike Exec, the session owns cancellation: no CommandContext Cancel
		// hook (which would reject a plain exec.Command), and no ctx kill — the
		// session lives until Terminate/Close.
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
	}
	proc := cmd.Process
	pgid := proc.Pid

	var sampler *GroupCapsWatcher
	if spec.Opts.Resources.MemoryBytes > 0 || spec.Opts.Resources.CPUMillicores > 0 {
		// The Start ctx only bounds the spawn; the session outlives it.
		// WithoutCancel keeps the watcher's telemetry rooted in the originating
		// trace without making a canceled Start kill the session.
		sampler = StartGroupCapsWatcher(context.WithoutCancel(ctx), pgid, spec.Opts.Resources, spec.Opts.Timeout)
	}

	var releaseOnce sync.Once
	s.port = port{
		pid:     func() int { return proc.Pid },
		caps:    sandbox.SessionCapabilities{TTY: tty, Stdin: s.input, Signal: true, Events: true},
		resize:  func(rows, cols int) error { return resizePTY(ptmx, &ptyMu, rows, cols) },
		signal:  func(sig sandbox.SessionSignal) error { return signalSession(ptmx, &ptyMu, pgid, sig) },
		soft:    func() { killGroup(pgid, syscall.SIGTERM, "sandbox: SIGTERM process group failed") },
		hard:    func() { killGroup(pgid, syscall.SIGKILL, "sandbox: SIGKILL process group failed") },
		wait:    func() (sandbox.SessionExit, error) { return waitSession(s, cmd, &ptmx, &ptyMu, &copiers, sampler) },
		release: func() { releaseOnce.Do(func() { releaseSession(s, &ptmx, &ptyMu, sampler) }) },
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

// resizePTY applies a window size. Setsize uses Fd(), which is not safe against a
// concurrent Close, so the lock spans the ioctl.
func resizePTY(ptmx *os.File, mu *sync.Mutex, rows, cols int) error {
	mu.Lock()
	defer mu.Unlock()
	if ptmx == nil {
		return errdefs.NotAvailablef("sandbox: Resize requires a TTY session")
	}
	return pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

// signalSession delivers Ctrl-C semantics: a VINTR byte on a TTY (the terminal
// driver signals the foreground group), SIGINT to the group on pipes.
func signalSession(ptmx *os.File, mu *sync.Mutex, pgid int, sig sandbox.SessionSignal) error {
	mu.Lock()
	master := ptmx
	mu.Unlock()
	if master != nil {
		// A no-op when the child put the terminal in raw mode (ISIG off) —
		// authentic Ctrl-C behaviour, documented on Session.
		if _, err := master.Write([]byte{0x03}); err != nil {
			return errdefs.Internal(fmt.Errorf("sandbox: signal write to pty: %w", err))
		}
		return nil
	}
	if err := syscall.Kill(-pgid, syscall.SIGINT); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return sandbox.ErrSessionClosed
		}
		return errdefs.Internal(fmt.Errorf("sandbox: signal process group: %w", err))
	}
	return nil
}

// killGroup delivers sig to the process group and leaves a failed delivery
// visible to telemetry. Failures during teardown are best-effort by contract
// (the child may already be gone), so they are never propagated to callers.
func killGroup(pgid int, sig syscall.Signal, msg string) {
	if err := syscall.Kill(-pgid, sig); err != nil {
		telemetry.WarnErr(context.Background(), msg, err,
			attribute.Int("sandbox.pgid", pgid))
	}
}

// waitSession reaps the child, closes the pty and classifies the outcome. It
// also waits for the pty copier, which may still hold bytes read before the
// child's last write flushed, so EOF is never reported early.
func waitSession(
	s *session,
	cmd *exec.Cmd,
	ptmx **os.File,
	mu *sync.Mutex,
	copiers *sync.WaitGroup,
	sampler *GroupCapsWatcher,
) (sandbox.SessionExit, error) {
	waitErr := cmd.Wait()
	mu.Lock()
	master := *ptmx
	*ptmx = nil
	mu.Unlock()
	if master != nil {
		if err := master.Close(); err != nil {
			telemetry.WarnErr(context.Background(), "sandbox: close pty after reap failed", err,
				attribute.String("sandbox.session_id", s.id))
		}
	}
	copiers.Wait()
	return classifyExit(s, sampler, waitErr)
}

// releaseSession frees the platform handles. It is called from reap and from
// Close, and runs once.
func releaseSession(s *session, ptmx **os.File, mu *sync.Mutex, sampler *GroupCapsWatcher) {
	if sampler != nil {
		sampler.Stop()
	}
	mu.Lock()
	master := *ptmx
	*ptmx = nil
	mu.Unlock()
	if master != nil {
		if err := master.Close(); err != nil {
			telemetry.WarnErr(context.Background(), "sandbox: close pty master failed", err,
				attribute.String("sandbox.session_id", s.id))
		}
	}
	s.mu.Lock()
	stdin := s.stdin
	s.stdin = nil
	s.mu.Unlock()
	if stdin != nil && stdin != master {
		if err := stdin.Close(); err != nil {
			telemetry.WarnErr(context.Background(), "sandbox: close session stdin failed", err,
				attribute.String("sandbox.session_id", s.id))
		}
	}
}

// classifyExit turns a wait result plus the session's own flags into what the
// contract reports.
func classifyExit(s *session, sampler *GroupCapsWatcher, waitErr error) (sandbox.SessionExit, error) {
	if sampler != nil {
		// Order matters: a watcher that gave up on sampling killed the process,
		// so its own verdict is what the caller has to see.
		if sampleErr := sampler.Unenforceable(); sampleErr != nil {
			return sandbox.SessionExit{Code: -1, Reason: sandbox.SessionBudgetExceeded},
				errdefs.NotAvailablef(
					"sandbox: resource caps became unenforceable while running process: %w", sampleErr)
		}
		if cap := sampler.Exceeded(); cap != "" {
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

// copyLoop drains the pty master into the output log as the TTY stream.
func copyLoop(out *outputLog, r io.Reader) {
	buf := make([]byte, sessionCopyChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			out.append(sandbox.SessionStreamTTY, buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// isBrokenPipe is the unix half of [isClosedInput]: a pipe whose reader is gone,
// or a descriptor this process already closed.
func isBrokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EBADF)
}
