package remote

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
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
	body, err := s.runner.retryBytes(ctx, OpRead, req)
	if err != nil {
		return sandbox.SessionOutput{}, err
	}
	out, idle, err := wire.DecodeReadResponse(body)
	if err != nil {
		return sandbox.SessionOutput{}, err
	}
	if idle {
		return sandbox.SessionOutput{NextSeq: afterSeq}, nil
	}
	return out, nil
}

// Write implements sandbox.Session.
//
// One attempt, deliberately: a retry after a failed write could run the bytes
// twice, and the contract says Write either writes all of them or fails.
func (s *session) Write(ctx context.Context, data []byte) error {
	if err := s.live(); err != nil {
		return err
	}
	body := wire.AppendWriteRequest(make([]byte, 0, wire.WriteRequestSize(s.id, data)), s.id, data)
	return s.runner.doEncoded(ctx, OpWrite, body, nil)
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
		if failures > 0 {
			if err := retrySleep(ctx, retryBackoff(failures-1)); err != nil {
				return sandbox.SessionExit{}, errdefs.FromContext(err)
			}
		}
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
			// means over a request/response transport. This is not a failure, so
			// it neither counts against the retry budget nor waits.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return sandbox.SessionExit{}, errdefs.FromContext(ctxErr)
			}
			failures = 0
		default:
			if ctxErr := ctx.Err(); ctxErr != nil {
				return sandbox.SessionExit{}, errdefs.FromContext(ctxErr)
			}
			if !isTransportError(err) {
				return sandbox.SessionExit{}, err
			}
			failures++
			if failures > s.runner.retries {
				return sandbox.SessionExit{}, err
			}
		}
	}
}

// Watch implements sandbox.Session: the server's push stream, framed as SSE.
//
// The stream is replay-then-live on the server, so this only forwards. A consumer
// that falls behind is not invented a Lag event here: the contract's lag comes
// from the server's own bounded queue, and a lag frame tells the consumer which
// seq to Read from.
func (s *session) Watch(ctx context.Context) (sandbox.SessionWatcher, error) {
	if err := s.live(); err != nil {
		return nil, err
	}
	if !s.caps.Events {
		return nil, errdefs.NotAvailablef(
			"sandbox-remote: session %q was not started with an event stream", s.id)
	}

	// The stream lives as long as the watcher, not as long as this call.
	streamCtx, cancel := context.WithCancel(ctx)
	body, err := s.runner.transport.Stream(streamCtx, OpEvents,
		mustEncode(wire.EventsRequest{ID: s.id}))
	if err != nil {
		cancel()
		return nil, err
	}

	w := &watcher{
		cancel: cancel,
		stream: body,
		events: make(chan sandbox.SessionEvent, watchBuffer),
	}
	go w.pump(streamCtx)
	return w, nil
}

// watchBuffer bounds the queue between the frames arriving and the consumer
// reading them. A full queue blocks the pump, which is backpressure on the
// connection rather than a dropped event.
const watchBuffer = 64

// watcher is one subscription to the server's event stream.
type watcher struct {
	cancel context.CancelFunc
	stream io.ReadCloser
	events chan sandbox.SessionEvent

	once sync.Once
}

// Events implements sandbox.SessionWatcher.
func (w *watcher) Events() <-chan sandbox.SessionEvent { return w.events }

// Close implements sandbox.SessionWatcher: it ends the request and closes the
// channel after the pump has stopped, so a reader never sees a send on a closed
// channel.
func (w *watcher) Close() error {
	w.once.Do(w.cancel)
	return nil
}

// pump decodes SSE frames until the stream ends, the context is cancelled, or
// the server sends the session's Closed event.
func (w *watcher) pump(ctx context.Context) {
	defer close(w.events)
	defer func() { _ = w.stream.Close() }()

	reader := bufio.NewReaderSize(w.stream, 64<<10)
	var (
		data  []byte
		frame bool
	)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if frame {
					done := w.deliver(ctx, data)
					data, frame = nil, false
					if done {
						return
					}
				}
			case strings.HasPrefix(line, "data:"):
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")...)
				frame = true
			}
			// id: and event: are redundant with the payload's own Seq and Type.
		}
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return
			}
			return
		}
	}
}

// deliver hands one frame to the consumer and reports whether the stream is
// finished (the Closed event is the last one).
func (w *watcher) deliver(ctx context.Context, data []byte) bool {
	var frame wire.Event
	if err := wire.Decode(data, &frame); err != nil {
		return true // a frame we cannot read ends the subscription
	}
	ev, err := wire.ToEvent(frame)
	if err != nil {
		return true
	}
	select {
	case w.events <- ev:
	case <-ctx.Done():
		return true
	}
	return ev.Type == sandbox.SessionEventClosed
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

var (
	_ sandbox.Session        = (*session)(nil)
	_ sandbox.SessionWatcher = (*watcher)(nil)
)
