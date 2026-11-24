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

	ptylib "github.com/creack/pty"
)

const (
	defaultSessionRows = 24
	defaultSessionCols = 80
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

	pty := &ptyHandle{}
	var copiers sync.WaitGroup
	tty := spec.TTY
	if tty {
		err := startWithCleanSignalMask(func() error {
			var startErr error
			master, startErr := ptylib.StartWithSize(cmd, &ptylib.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
			if startErr == nil {
				pty.set(master)
			}
			return startErr
		})
		if err != nil {
			return nil, classifyStartError(cmd.Path, err)
		}
		s.stdin = pty.get()
		copiers.Add(1)
		go func() {
			defer copiers.Done()
			copyLoop(s.out, pty.get())
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
						telemetry.String("sandbox.session_id", s.id))
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
		resize:  func(rows, cols int) error { return resizePTY(pty, rows, cols) },
		signal:  func(sig sandbox.SessionSignal) error { return signalSession(pty, pgid, sig) },
		soft:    func() { killGroup(pgid, syscall.SIGTERM, "sandbox: SIGTERM process group failed") },
		hard:    func() { killGroup(pgid, syscall.SIGKILL, "sandbox: SIGKILL process group failed") },
		wait:    func() (sandbox.SessionExit, error) { return waitSession(s, cmd, pty, &copiers, sampler, pgid) },
		release: func() { releaseOnce.Do(func() { releaseSession(s, pty, sampler) }) },
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
// ptyHandle owns the master side of a pty: the copy loop, the ioctl and reap all
// reach it from different goroutines, so the value lives behind one lock instead
// of in a variable they share.
type ptyHandle struct {
	mu   sync.Mutex
	file *os.File
}

func (p *ptyHandle) set(file *os.File) {
	p.mu.Lock()
	p.file = file
	p.mu.Unlock()
}

func (p *ptyHandle) get() *os.File {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.file
}

// take returns the master and forgets it, so exactly one caller closes it.
func (p *ptyHandle) take() *os.File {
	p.mu.Lock()
	defer p.mu.Unlock()
	file := p.file
	p.file = nil
	return file
}

func resizePTY(pty *ptyHandle, rows, cols int) error {
	// Setsize uses Fd(), which is not safe against a concurrent Close, so the
	// lock spans the ioctl.
	pty.mu.Lock()
	defer pty.mu.Unlock()
	if pty.file == nil {
		return errdefs.NotAvailablef("sandbox: Resize requires a TTY session")
	}
	return ptylib.Setsize(pty.file, &ptylib.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

// signalSession delivers Ctrl-C semantics: a VINTR byte on a TTY (the terminal
// driver signals the foreground group), SIGINT to the group on pipes.
func signalSession(pty *ptyHandle, pgid int, sig sandbox.SessionSignal) error {
	master := pty.get()
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
			telemetry.Int("sandbox.pgid", pgid))
	}
}

// ptyDrainGrace bounds each step of the drain once the child is reaped.
const ptyDrainGrace = 500 * time.Millisecond

// drainAndClosePTY hands the copier what the child left behind, then closes the
// master.
//
// The ordering is the fix: when a child is reaped its last write can still sit in
// the pty buffer, and the copier is the only reader that turns those bytes into
// journal chunks, so closing the master first discards them.
//
// It is bounded because closing a pty master does not interrupt a read that is
// already blocked — not on Linux, not on macOS — so a child that left something
// holding the slave (a daemon behind `trap ” HUP`) would otherwise leave the
// copier waiting on a descriptor nobody will close, and Wait would never return.
// A terminal hangup is what ends such a group in the ordinary case, so the group
// gets SIGHUP; if that is ignored it gets SIGKILL, which does close the
// descriptors and lets the copier's read fail. The old order hung in exactly this
// case, because it relied on the close to interrupt the read.
func drainAndClosePTY(copiers *sync.WaitGroup, pty *ptyHandle, pgid int, id string) {
	done := make(chan struct{})
	go func() {
		copiers.Wait()
		close(done)
	}()

	if !waitClosed(done, ptyDrainGrace) {
		killGroup(pgid, syscall.SIGHUP, "sandbox: SIGHUP the session group after reap failed")
		if !waitClosed(done, ptyDrainGrace) {
			killGroup(pgid, syscall.SIGKILL, "sandbox: SIGKILL the session group after reap failed")
			waitClosed(done, ptyDrainGrace)
		}
	}

	master := pty.take()
	if master != nil {
		if err := master.Close(); err != nil {
			telemetry.WarnErr(context.Background(), "sandbox: close pty after reap failed", err,
				telemetry.String("sandbox.session_id", id))
		}
	}
	// Idempotent when the copier finished above. After a kill it is the step that
	// keeps the journal consistent: the log is finished only once no copier can
	// append again.
	copiers.Wait()
}

// waitClosed reports whether done was closed within d.
func waitClosed(done <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// waitSession reaps the child, drains the pty and classifies the outcome.
//
// The drain is bounded (see [ptyDrainGrace]): a copier whose slave is still held
// by a backgrounded grandchild is released by closing the master, so EOF can
// arrive while that grandchild lives. What the bound buys is the other case —
// bytes the child wrote but nobody had read yet — and that is the one a caller
// notices.
func waitSession(
	s *session,
	cmd *exec.Cmd,
	pty *ptyHandle,
	copiers *sync.WaitGroup,
	sampler *GroupCapsWatcher,
	pgid int,
) (sandbox.SessionExit, error) {
	waitErr := cmd.Wait()
	// A verdict is what the sampler observed while the child ran. Reading it here
	// keeps the drain below from attributing a background grandchild's memory to a
	// child that has already exited.
	sampled := readVerdict(sampler)
	drainAndClosePTY(copiers, pty, pgid, s.id)
	return classifyExit(s, sampled, waitErr)
}

// releaseSession frees the platform handles. It is called from reap and from
// Close, and runs once.
func releaseSession(s *session, pty *ptyHandle, sampler *GroupCapsWatcher) {
	if sampler != nil {
		sampler.Stop()
	}
	master := pty.take()
	if master != nil {
		if err := master.Close(); err != nil {
			telemetry.WarnErr(context.Background(), "sandbox: close pty master failed", err,
				telemetry.String("sandbox.session_id", s.id))
		}
	}
	s.mu.Lock()
	stdin := s.stdin
	s.stdin = nil
	s.mu.Unlock()
	if stdin != nil && stdin != master {
		if err := stdin.Close(); err != nil {
			telemetry.WarnErr(context.Background(), "sandbox: close session stdin failed", err,
				telemetry.String("sandbox.session_id", s.id))
		}
	}
}

// classifyExit turns a wait result plus the session's own flags into what the
// contract reports.
// capsVerdict is what the resource sampler concluded while the child ran.
type capsVerdict struct {
	unenforceable error
	exceeded      string
}

// readVerdict reads the sampler once, at reap time.
func readVerdict(sampler *GroupCapsWatcher) capsVerdict {
	if sampler == nil {
		return capsVerdict{}
	}
	return capsVerdict{unenforceable: sampler.Unenforceable(), exceeded: sampler.Exceeded()}
}

func classifyExit(s *session, sampled capsVerdict, waitErr error) (sandbox.SessionExit, error) {
	if sampled.unenforceable != nil || sampled.exceeded != "" {
		// Order matters: a watcher that gave up on sampling killed the process, so
		// its own verdict is what the caller has to see — and it is not a budget
		// verdict. Nothing was measured, so the reason names what happened: a host
		// that retries a breach differently from a broken guarantee reads it, and
		// the error type says the same thing to a caller that only inspects errors.
		if sampleErr := sampled.unenforceable; sampleErr != nil {
			return sandbox.SessionExit{Code: -1, Reason: sandbox.SessionUnenforceable},
				errdefs.NotAvailablef(
					"sandbox: resource caps became unenforceable while running process: %w", sampleErr)
		}
		if cap := sampled.exceeded; cap != "" {
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

// SessionFeatures reports what a session supports on this platform. A backend
// advertises these and the session reports the same, so a caller can trust the
// runner-level answer before starting anything.
func SessionFeatures() sandbox.SessionFeatures {
	return sandbox.SessionFeatures{TTY: true, Signal: true, Events: true}
}

// isBrokenPipe is the unix half of [isClosedInput]: a pipe whose reader is gone,
// or a descriptor this process already closed.
func isBrokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EBADF)
}
