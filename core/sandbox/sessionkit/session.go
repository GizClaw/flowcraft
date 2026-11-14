package sessionkit

// The session state machine: one implementation of the sandbox.Session
// contract, shared by every platform. What a platform actually does differently
// — how it starts a child, how it kills a process tree, whether it has a pty,
// what its exit status looks like — is in the port, a set of functions the
// spawner fills in. Nothing here branches on GOOS.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel/attribute"
)

const (
	sessionTerminateGrace   = 2 * time.Second
	sessionWriteConcurrency = 4
	sessionKillTimeout      = 2 * time.Second
)

// isClosedInput reports whether a write failed because the far end is no longer
// reading: the child closed its stdin, or the session's own descriptor is gone.
// Both mean "stop writing", not "the write broke".
func isClosedInput(err error) bool {
	return errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, os.ErrClosed) ||
		isBrokenPipe(err)
}

// port is the platform half of a session.
//
// The state machine calls these and never inspects them: a session with no pty
// has a nil resize, one with no signal delivery a nil signal, and a platform
// without a process tree still provides soft/hard as "stop the child".
type port struct {
	// pid is the child's process id, or 0 before it exists.
	pid func() int
	// caps reports what this session actually supports.
	caps sandbox.SessionCapabilities
	// resize applies a window size. Nil means no pty.
	resize func(rows, cols int) error
	// signal delivers an interactive signal. Nil means the platform has none.
	signal func(sig sandbox.SessionSignal) error
	// soft asks the process tree to stop (SIGTERM / CTRL_BREAK).
	soft func()
	// hard terminates the process tree (SIGKILL to the group / job object).
	hard func()
	// wait blocks until the child exits and classifies the outcome.
	wait func() (sandbox.SessionExit, error)
	// release frees the platform handles: pty, job, token, sampler. It must be
	// safe to call more than once — both reap and Close call it.
	release func()
}

// session is the concrete backend session: it owns the child's input side, the
// bounded replayable output log, and the lifecycle the contract promises.
type session struct {
	id         string
	stdin      io.WriteCloser
	input      bool // an input side exists: Write has somewhere to go
	out        *outputLog
	writeSlots chan struct{}

	mu         sync.Mutex
	closed     bool
	timedOut   bool
	terminated bool
	exit       sandbox.SessionExit
	waitErr    error
	done       chan struct{}

	port port
}

func (s *session) ID() string { return s.id }

func (s *session) PID() int {
	if s.port.pid == nil {
		return 0
	}
	return s.port.pid()
}

func (s *session) Read(ctx context.Context, afterSeq int64, maxBytes int) (sandbox.SessionOutput, error) {
	if s.isClosed() {
		return sandbox.SessionOutput{}, sandbox.ErrSessionClosed
	}
	return s.out.read(ctx, afterSeq, maxBytes)
}

func (s *session) Write(ctx context.Context, data []byte) error {
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

// CloseInput closes the session's stdin. TTY sessions cannot close their input
// (the pty is bidirectional) and return NotAvailable; a pipe session started
// without an input side is a no-op success, so Exec can call it
// unconditionally.
func (s *session) CloseInput() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sandbox.ErrSessionClosed
	}
	if s.port.caps.TTY {
		return errdefs.NotAvailablef(
			"sandbox: cannot close input on a TTY session")
	}
	if s.stdin == nil {
		return nil
	}
	err := s.stdin.Close()
	s.stdin = nil
	if errors.Is(err, os.ErrClosed) {
		// Wait closes the parent-side stdin write end once the child exits.
		// Racing that cleanup is a successful no-op: the input is already
		// closed, which is the desired state.
		return nil
	}
	return err
}

func (s *session) Resize(_ context.Context, rows, cols int) error {
	if rows <= 0 || cols <= 0 {
		return errdefs.Validationf("sandbox: rows and cols must be positive")
	}
	if s.isClosed() {
		return sandbox.ErrSessionClosed
	}
	if s.port.resize == nil {
		return errdefs.NotAvailablef("sandbox: Resize requires a TTY session")
	}
	return s.port.resize(rows, cols)
}

func (s *session) Terminate(ctx context.Context) error {
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
	return s.stop(ctx, false)
}

// Signal implements Session: interrupt means Ctrl-C semantics. The process may
// catch the signal and continue, so the session stays usable.
func (s *session) Signal(_ context.Context, sig sandbox.SessionSignal) error {
	if sig != sandbox.SessionSignalInterrupt || s.port.signal == nil {
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
	return s.port.signal(sig)
}

// Watch implements Session: it subscribes an independent bounded queue that
// replays the retained output before delivering live events. The channel closes
// on ctx cancellation, watcher.Close, or right after the process's Closed event.
func (s *session) Watch(ctx context.Context) (sandbox.SessionWatcher, error) {
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
		// Replay alone would overflow the queue; skip the partial replay and
		// deliver one Lag from the retained start instead.
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

// Capabilities reports what this session actually supports, as the platform
// declared it at spawn.
func (s *session) Capabilities() sandbox.SessionCapabilities { return s.port.caps }

func (s *session) Wait(ctx context.Context) (sandbox.SessionExit, error) {
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

func (s *session) Close() error {
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
		s.port.hard()
		select {
		case <-s.done:
		case <-time.After(sessionKillTimeout):
			telemetry.Warn(context.Background(),
				"sandbox: process tree did not exit after the kill signal on close",
				attribute.String("sandbox.session_id", s.id))
		}
	}
	s.port.release()
	s.out.close()
	return nil
}

func (s *session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// timeoutKill is the ExecOptions.Timeout enforcement: the session is killed
// hard, like Exec does on a context deadline.
func (s *session) timeoutKill() {
	s.mu.Lock()
	s.timedOut = true
	s.mu.Unlock()
	_ = s.stop(context.Background(), true)
}

// stop ends the session. force kills the tree directly (timeout / close path);
// otherwise the tree is asked to stop first, with a short grace period bounded
// by ctx.
func (s *session) stop(ctx context.Context, force bool) error {
	if force {
		s.port.hard()
		return nil
	}
	s.port.soft()

	grace := sessionTerminateGrace
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < grace {
			grace = remaining
		}
	}
	if grace <= 0 {
		s.port.hard()
		return ctx.Err()
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-s.done:
		return nil
	case <-timer.C:
		s.port.hard()
		return nil
	case <-ctx.Done():
		s.port.hard()
		return ctx.Err()
	}
}

// reap waits for the child, classifies the outcome, then finishes the output
// log. The platform's wait also drains its own output copiers, so finish() never
// races buffered output.
func (s *session) reap() {
	exit, err := s.port.wait()
	s.port.release()
	s.mu.Lock()
	s.exit = exit
	s.waitErr = err
	s.mu.Unlock()
	s.out.finish(s.exit)
	close(s.done)
}
