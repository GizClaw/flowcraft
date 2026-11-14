//go:build windows

package sessionkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/rs/xid"

	xwin "golang.org/x/sys/windows"
)

// ProcessSetup applies a backend's process policy to the command it is about to
// start — the token, the AppContainer, whatever the platform needs — and returns
// a release that runs when the session ends. The kit owns the session; the
// backend owns the policy.
//
// It is applied to pipe sessions. A TTY session is spawned by the pseudo console
// (which builds its own process attributes), so a backend that needs a policy on
// a TTY session must refuse it rather than assume this ran.
type ProcessSetup func(cmd *exec.Cmd) (release func() error, err error)

// StartSession starts one session over the Windows spawn path: pipes with a job
// object, or a pseudo console when the spec asks for a TTY.
func StartSession(ctx context.Context, spec sandbox.SessionSpec, cmd *exec.Cmd) (sandbox.Session, error) {
	return StartSessionWithPolicy(ctx, spec, cmd, nil)
}

// StartSessionWithPolicy is [StartSession] under a backend's own policy.
func StartSessionWithPolicy(
	ctx context.Context,
	spec sandbox.SessionSpec,
	cmd *exec.Cmd,
	setup ProcessSetup,
) (sandbox.Session, error) {
	if cmd == nil {
		return nil, errdefs.Validationf("sandbox: nil command for process session")
	}
	id := spec.ID
	if id == "" {
		id = xid.New().String()
	}

	j, err := createJob(spec.Opts.Resources, spec.Opts.Timeout)
	if err != nil {
		return nil, err
	}
	abort := func(err error) (sandbox.Session, error) {
		// KILL_ON_JOB_CLOSE terminates the child if it already ran.
		if cerr := j.close(); cerr != nil {
			telemetry.WarnErr(context.Background(),
				"windows: close job after start failure", cerr)
		}
		return nil, err
	}

	s := &session{
		id:         id,
		out:        newOutputLog(spec.Opts.Resources.MaxOutputBytes),
		input:      spec.Stdin || spec.TTY,
		done:       make(chan struct{}),
		writeSlots: make(chan struct{}, sessionWriteConcurrency),
	}

	var policyRelease func() error
	if setup != nil && spec.TTY {
		// A pseudo console spawns the process itself, with its own attributes,
		// so a policy cannot be attached to it.
		return abort(errdefs.NotAvailablef(
			"windows: a TTY session cannot carry a process policy; use a pipe session"))
	}

	var (
		pty    *conpty
		procH  xwin.Handle
		pid    int
		waited chan struct{}
		once   sync.Once
	)
	release := func() {
		once.Do(func() {
			if policyRelease != nil {
				if err := policyRelease(); err != nil {
					telemetry.WarnErr(context.Background(),
						"windows: release process policy", err)
				}
			}
			if pty != nil {
				_ = pty.close()
			}
			if procH != 0 {
				_ = xwin.CloseHandle(procH)
			}
			if cerr := j.close(); cerr != nil {
				telemetry.WarnErr(context.Background(),
					"windows: close job", cerr)
			}
		})
	}

	if spec.TTY {
		rows, cols := uint32(spec.Rows), uint32(spec.Cols)
		if rows == 0 {
			rows = defaultTTYRows
		}
		if cols == 0 {
			cols = defaultTTYCols
		}
		pty, err = newConPTY(rows, cols)
		if err != nil {
			return abort(err)
		}
		procH, pid, err = pty.spawn(cmd.Args, cmd.Dir, cmd.Env)
		if err != nil {
			_ = pty.close()
			return abort(classifyStartError(cmd.Path, err))
		}
		s.stdin = pty.in
		if err := j.assign(pid); err != nil {
			// The child never ran; make sure it cannot linger suspended.
			_ = xwin.TerminateProcess(procH, 1)
			return abort(err)
		}
		if err := resumeProcess(pid); err != nil {
			_ = xwin.TerminateProcess(procH, 1)
			return abort(err)
		}
		waited = make(chan struct{})
		go func() {
			defer close(waited)
			copyLoop(s.out, pty.out)
		}()
	} else {
		if s.input {
			stdin, pipeErr := cmd.StdinPipe()
			if pipeErr != nil {
				return abort(classifyStartError(cmd.Path, pipeErr))
			}
			s.stdin = stdin
		}
		cmd.Stdout = sessionWriter{out: s.out, stream: sandbox.SessionStreamStdout}
		cmd.Stderr = sessionWriter{out: s.out, stream: sandbox.SessionStreamStderr}
		// os/exec copies child stdout/stderr into these writers, and cmd.Wait
		// drains them, so output ordering relative to process exit is exact.
		cmd.SysProcAttr = &xwin.SysProcAttr{
			CreationFlags: xwin.CREATE_SUSPENDED | xwin.CREATE_NO_WINDOW,
		}
		// The policy runs after the struct exists: it sets the token on it, and
		// building the struct afterwards would drop it.
		if setup != nil {
			policyRelease, err = setup(cmd)
			if err != nil {
				return abort(err)
			}
		}
		if err := cmd.Start(); err != nil {
			if s.stdin != nil {
				_ = s.stdin.Close()
			}
			return abort(classifyStartError(cmd.Path, err))
		}
		pid = cmd.Process.Pid
		if err := j.assign(pid); err != nil {
			return abort(err)
		}
		if err := resumeProcess(pid); err != nil {
			return abort(err)
		}
	}

	s.port = port{
		pid: func() int { return pid },
		caps: sandbox.SessionCapabilities{
			TTY:    spec.TTY,
			Stdin:  s.input,
			Signal: false, // a job has no signal channel
			Events: true,
		},
		resize: func(rows, cols int) error {
			if pty == nil {
				return errdefs.NotAvailablef("sandbox: Resize requires a TTY session")
			}
			return pty.resize(uint32(rows), uint32(cols))
		},
		signal: nil,
		// A job object has one way to stop a process tree: terminate it. There is
		// no SIGTERM to send first, so both are the same call and the state
		// machine's grace period simply elapses.
		soft: func() { terminateJob(j) },
		hard: func() { terminateJob(j) },
		wait: func() (sandbox.SessionExit, error) {
			var waitErr error
			if spec.TTY {
				waitErr = waitTTY(procH, pty, waited)
			} else {
				waitErr = cmd.Wait()
			}
			return classifyExit(s, j, waitErr)
		},
		release: release,
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

// terminateJob kills the whole tree. Failures are best-effort: the processes may
// already be gone.
func terminateJob(j *job) {
	if err := j.terminate(); err != nil {
		telemetry.WarnErr(context.Background(), "windows: terminate job", err)
	}
}

// waitTTY waits for a pseudo-console child, drains its output and returns the
// exit as an error, so classifyExit sees the same shape as a pipe session.
func waitTTY(procH xwin.Handle, pty *conpty, copied chan struct{}) error {
	if _, err := xwin.WaitForSingleObject(procH, xwin.INFINITE); err != nil {
		return errdefs.Internal(fmt.Errorf("windows: wait tty process: %w", err))
	}
	var code uint32
	if err := xwin.GetExitCodeProcess(procH, &code); err != nil {
		return errdefs.Internal(fmt.Errorf("windows: read tty exit code: %w", err))
	}
	// Releasing the console emits the final output frame and closes the output
	// channel; drain it before finishing the log.
	pty.releaseConsole()
	select {
	case <-copied:
	case <-time.After(sessionKillTimeout):
		// Force a stuck copy read out of the pipe.
		_ = pty.close()
		<-copied
	}
	if code == 0 {
		return nil
	}
	return &ttyExitError{code: int(code)}
}

// classifyExit turns a wait result plus the session's own flags into what the
// contract reports.
func classifyExit(s *session, j *job, waitErr error) (sandbox.SessionExit, error) {
	s.mu.Lock()
	timedOut := s.timedOut
	terminated := s.terminated
	s.mu.Unlock()
	// A cap-kill completion message can land a moment after the process exits;
	// give the notifier a short grace window so a kernel-enforced limit is
	// classified as BudgetExceeded rather than a plain exit.
	if j.notify != nil {
		deadline := time.Now().Add(100 * time.Millisecond)
		for j.budgetCap() == "" && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if cap := j.budgetCap(); cap != "" {
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

// SessionFeatures reports what a session supports on this platform: a pseudo
// console gives a TTY and a merged output stream, and a job object has no signal
// channel to deliver an interrupt through.
func SessionFeatures() sandbox.SessionFeatures {
	return sandbox.SessionFeatures{TTY: true, Events: true}
}

// isBrokenPipe is the Windows half of [isClosedInput]: the child closed its
// stdin, or the pipe is being closed under us.
func isBrokenPipe(err error) bool {
	return errors.Is(err, xwin.ERROR_BROKEN_PIPE) || errors.Is(err, xwin.ERROR_NO_DATA)
}

// osFileStdin adapts an *os.File to the io.WriteCloser the state machine writes
// to; *_os.File already satisfies it, and this keeps that explicit for the pty
// branch where the value is a pipe end.
var _ io.WriteCloser = (*os.File)(nil)
