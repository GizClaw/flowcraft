package remote

import (
	"context"
	"errors"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-remote/wire"
)

// session is the client-side handle to one remote process.
//
// It holds no output: the log lives on the server, and a read is a cursor
// message. That is what makes a reconnect cheap — the handle can be rebuilt from
// the id at any retained position.
type session struct {
	runner *Runner
	id     string
	pid    int
	caps   sandbox.SessionCapabilities

	mu     sync.Mutex
	closed bool
	exit   *sandbox.SessionExit
}

// ID implements sandbox.Session.
func (s *session) ID() string { return s.id }

// PID implements sandbox.Session. It is the process id on the server's host and
// is informational only: it names nothing on this side of the wire.
func (s *session) PID() int { return s.pid }

// Capabilities implements sandbox.Session: what the server declared for this
// particular session, verbatim.
func (s *session) Capabilities() sandbox.SessionCapabilities { return s.caps }

// Read implements sandbox.Session.
//
// A hold that elapses on the server comes back as Idle with the cursor
// unchanged; this returns an empty batch then, so a caller's read loop simply
// asks again. Nothing is ever skipped: after_seq is exclusive and the server
// answers with the same cursor when it has nothing.
func (s *session) Read(ctx context.Context, afterSeq int64, maxBytes int) (sandbox.SessionOutput, error) {
	if err := s.live(); err != nil {
		return sandbox.SessionOutput{}, err
	}
	req := wire.ReadRequest{
		ID:       s.id,
		AfterSeq: afterSeq,
		MaxBytes: maxBytes,
		HoldMS:   s.runner.holdMS(),
	}
	var out wire.ReadResponse
	if err := s.runner.retry(ctx, OpRead, req, &out); err != nil {
		return sandbox.SessionOutput{}, err
	}
	if out.Idle {
		return sandbox.SessionOutput{NextSeq: afterSeq}, nil
	}
	return out.Output, nil
}

// Write implements sandbox.Session.
//
// One attempt, deliberately: a retry after a failed write could run the bytes
// twice, and the contract says Write either writes all of them or fails.
func (s *session) Write(ctx context.Context, data []byte) error {
	if err := s.live(); err != nil {
		return err
	}
	return s.runner.do(ctx, OpWrite, wire.WriteRequest{ID: s.id, Data: data}, nil)
}

// CloseInput implements sandbox.Session.
//
// The interface takes no context, so this bounds its own request: an
// uncancellable call that can hang is worse than a timeout a caller can retry.
func (s *session) CloseInput() error {
	if err := s.live(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	return s.runner.do(ctx, OpCloseInput, wire.SessionRef{ID: s.id}, nil)
}

// Resize implements sandbox.Session.
func (s *session) Resize(ctx context.Context, rows, cols int) error {
	if err := s.live(); err != nil {
		return err
	}
	return s.runner.do(ctx, OpResize, wire.ResizeRequest{ID: s.id, Rows: rows, Cols: cols}, nil)
}

// Signal implements sandbox.Session.
func (s *session) Signal(ctx context.Context, sig sandbox.SessionSignal) error {
	if err := s.live(); err != nil {
		return err
	}
	return s.runner.do(ctx, OpSignal, wire.SignalRequest{ID: s.id, Signal: int(sig)}, nil)
}

// Terminate implements sandbox.Session.
func (s *session) Terminate(ctx context.Context) error {
	if err := s.live(); err != nil {
		return err
	}
	return s.runner.do(ctx, OpTerminate, wire.SessionRef{ID: s.id}, nil)
}

// Wait implements sandbox.Session.
//
// The exit is cached on both sides, so this asks until the server stops
// answering "still running" and then remembers the answer: later calls cost
// nothing and keep working after Close, exactly as the contract promises.
func (s *session) Wait(ctx context.Context) (sandbox.SessionExit, error) {
	s.mu.Lock()
	cached := s.exit
	s.mu.Unlock()
	if cached != nil {
		return *cached, nil
	}

	failures := 0
	for {
		var out wire.WaitResponse
		err := s.runner.transport.Do(ctx, OpWait,
			mustEncode(wire.WaitRequest{ID: s.id, HoldMS: s.runner.holdMS()}), &out)
		switch {
		case err == nil && out.Exit != nil:
			s.mu.Lock()
			s.exit = out.Exit
			s.mu.Unlock()
			return *out.Exit, nil
		case err == nil:
			// The server held as long as it was asked to and the process is
			// still alive: ask again, which is what "blocks until it exits"
			// means over a request/response transport.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return sandbox.SessionExit{}, errdefs.FromContext(ctxErr)
			}
			failures = 0
		default:
			if !isTransportError(err) || ctx.Err() != nil {
				return sandbox.SessionExit{}, err
			}
			failures++
			if failures > s.runner.retries {
				return sandbox.SessionExit{}, err
			}
		}
	}
}

// Watch implements sandbox.Session.
//
// The push stream is the one part of the contract this client does not carry
// yet, and its Capabilities say so (Events is cleared). Returning NotAvailable
// here is the honest answer; a silent empty channel would look like a quiet
// process.
func (s *session) Watch(ctx context.Context) (sandbox.SessionWatcher, error) {
	return nil, errdefs.NotAvailablef(
		"sandbox-remote: this client does not proxy the session event stream")
}

// Close implements sandbox.Session: it terminates the process, reaps it and
// releases the handle on both sides. Idempotent.
func (s *session) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	return s.close(ctx)
}

func (s *session) close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	s.runner.forget(s.id)

	var out wire.CloseResponse
	err := s.runner.transport.Do(ctx, OpClose, mustEncode(wire.SessionRef{ID: s.id}), &out)
	if err != nil && errors.Is(err, sandbox.ErrSessionClosed) {
		return nil
	}
	if err == nil && out.Exit != nil {
		// The reap already happened; remembering it is what makes Wait usable
		// after Close.
		s.mu.Lock()
		s.exit = out.Exit
		s.mu.Unlock()
	}
	return err
}

// live reports the handle-lifecycle error the contract defines: after Close,
// every operation but Wait refuses with ErrSessionClosed.
func (s *session) live() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sandbox.ErrSessionClosed
	}
	return nil
}

func mustEncode(v any) []byte {
	body, err := wire.Encode(v)
	if err != nil {
		// Encoding our own fixed-shape values cannot fail; if it somehow does,
		// sending nothing lets the server answer with a validation error rather
		// than panicking a live session.
		return nil
	}
	return body
}

var _ sandbox.Session = (*session)(nil)
