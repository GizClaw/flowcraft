package remote

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-remote/wire"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// Defaults for one server. They are constants rather than options because they
// bound what a client can make the server hold, not a policy choice: a hold is
// one goroutine and one request slot per session.
const (
	maxRequestBytes   = 8 << 20
	defaultServerHold = 25 * time.Second
	maxHold           = 60 * time.Second
)

// Server exposes one sandbox.Runner over the wire.
//
// It owns a handle per session it started or attached, because every operation
// after start addresses a session by id and the inner runner may have no way to
// hand one back (only backends that implement sandbox.SessionAttacher can). The
// handle table is also what lets the server answer "unknown session" with a
// NotFound instead of guessing.
//
// The server does not own the runner: Close releases the handles and the journal
// reader this server opened, and leaves the runner to whoever built it.
type Server struct {
	runner sandbox.Runner
	token  string
	open   bool // auth disabled by WithoutAuth
	name   string
	hold   time.Duration
	mux    *http.ServeMux

	mu      sync.Mutex
	handles map[string]sandbox.Session

	journalOnce sync.Once
	journal     sandbox.FileJournal
	journalErr  error
}

// ServerOption configures a Server.
type ServerOption func(*Server)

// WithServerToken requires "Authorization: Bearer <token>" on every request.
// A server without a token and without WithoutAuth refuses to be built: an
// unauthenticated command execution endpoint is not a default anyone should get
// by forgetting an option.
func WithServerToken(token string) ServerOption {
	return func(s *Server) { s.token = token }
}

// WithoutAuth serves without authentication. It exists for a loopback listener
// behind another authenticator (a unix socket, a sidecar that has already
// authenticated the caller); it says so out loud instead of being the default.
func WithoutAuth() ServerOption {
	return func(s *Server) { s.open = true }
}

// WithServerName identifies this server in the hello response.
func WithServerName(name string) ServerOption {
	return func(s *Server) { s.name = name }
}

// WithMaxHold bounds how long a client may ask the server to hold a read or a
// wait open. It is a resource bound: each held request is a goroutine.
func WithMaxHold(d time.Duration) ServerOption {
	return func(s *Server) {
		if d > 0 {
			s.hold = min(d, maxHold)
		}
	}
}

// NewServer exposes runner over the wire.
func NewServer(runner sandbox.Runner, opts ...ServerOption) (*Server, error) {
	if runner == nil {
		return nil, errdefs.Validationf("sandbox-remote: a server needs a runner")
	}
	s := &Server{
		runner:  runner,
		hold:    defaultServerHold,
		handles: make(map[string]sandbox.Session),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	if s.token == "" && !s.open {
		return nil, errdefs.Validationf(
			"sandbox-remote: refuse to serve command execution without authentication; " +
				"set WithServerToken or say WithoutAuth")
	}
	s.routes()
	return s, nil
}

// routes wires one handler per operation. Every operation is a POST to
// /v1/<op>: the transport addresses operations rather than resources, so the
// session id travels in the body and this table stays a closed set.
func (s *Server) routes() {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/hello", s.handleHello)
	mux.HandleFunc("POST /v1/start", s.handleStart)
	mux.HandleFunc("POST /v1/attach", s.handleAttach)
	mux.HandleFunc("POST /v1/list", s.handleList)
	mux.HandleFunc("POST /v1/read", s.handleRead)
	mux.HandleFunc("POST /v1/write", s.handleWrite)
	mux.HandleFunc("POST /v1/close-input", s.handleCloseInput)
	mux.HandleFunc("POST /v1/resize", s.handleResize)
	mux.HandleFunc("POST /v1/signal", s.handleSignal)
	mux.HandleFunc("POST /v1/terminate", s.handleTerminate)
	mux.HandleFunc("POST /v1/wait", s.handleWait)
	mux.HandleFunc("POST /v1/close", s.handleClose)
	mux.HandleFunc("POST /v1/journal", s.handleJournal)
	mux.HandleFunc("POST /v1/events", s.handleEvents)
	s.mux = mux
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.open && !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="sandbox"`)
		writeError(w, errdefs.Unauthorizedf("sandbox-remote: missing or wrong bearer token"))
		return
	}
	s.mux.ServeHTTP(w, r)
}

// Close releases the handles and the journal reader this server opened. The
// runner itself belongs to the caller: closing it here would take down a shared
// service.
func (s *Server) Close() error {
	s.mu.Lock()
	handles := make([]sandbox.Session, 0, len(s.handles))
	for id, h := range s.handles {
		handles = append(handles, h)
		delete(s.handles, id)
	}
	s.mu.Unlock()

	var err error
	for _, h := range handles {
		if cerr := h.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	if s.journal != nil {
		if jerr := s.journal.Close(); jerr != nil && err == nil {
			err = jerr
		}
	}
	return err
}

// ---------------------------------------------------------------------------
// operations
// ---------------------------------------------------------------------------

func (s *Server) handleHello(w http.ResponseWriter, r *http.Request) {
	// The capabilities are the runner's own value, verbatim. A proxy that
	// edited them would turn "ask before trying" into a failed call.
	writeJSON(w, wire.Hello{
		Version:      wire.Version,
		Server:       s.name,
		Capabilities: s.runner.Capabilities(),
	})
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var spec wire.Spec
	if !decodeRequest(w, r, &spec) {
		return
	}
	session, err := s.runner.Start(r.Context(), wire.ToSpec(spec))
	if err != nil {
		writeError(w, err)
		return
	}
	s.register(session)
	writeJSON(w, wire.StartResponse{
		ID:           session.ID(),
		PID:          session.PID(),
		Capabilities: session.Capabilities(),
	})
}

func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	var ref wire.SessionRef
	if !decodeRequest(w, r, &ref) {
		return
	}
	session, err := sandbox.Attach(r.Context(), s.runner, ref.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	s.register(session)
	writeJSON(w, wire.StartResponse{
		ID:           session.ID(),
		PID:          session.PID(),
		Capabilities: session.Capabilities(),
	})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	list, err := s.runner.List(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, list)
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	var req wire.ReadRequest
	if !decodeRequest(w, r, &req) {
		return
	}
	session, err := s.session(req.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	// A hold is a bounded wait for output: the session's Read blocks, so the
	// server bounds it with a deadline and reports the timeout as an idle
	// batch rather than as a failure.
	ctx, cancel := context.WithTimeout(r.Context(), s.holdFor(req.HoldMS))
	defer cancel()

	output, err := session.Read(ctx, req.AfterSeq, req.MaxBytes)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && r.Context().Err() == nil {
			// The hold elapsed: an idle frame at the same cursor, so the caller
			// simply asks again.
			writeReadResponse(w, sandbox.SessionOutput{NextSeq: req.AfterSeq}, true)
			return
		}
		writeError(w, err)
		return
	}
	writeReadResponse(w, output, false)
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	body, ok := readRequestBody(w, r)
	if !ok {
		return
	}
	id, data, err := wire.DecodeWriteRequest(body)
	if err != nil {
		writeError(w, err)
		return
	}
	session, err := s.session(id)
	if err != nil {
		writeError(w, err)
		return
	}
	// data points into body; Write sends it without a second copy.
	if err := session.Write(r.Context(), data); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, wire.WriteResponse{BytesWritten: len(data)})
}

func (s *Server) handleCloseInput(w http.ResponseWriter, r *http.Request) {
	session, ok := s.sessionFor(w, r)
	if !ok {
		return
	}
	if err := session.CloseInput(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, struct{}{})
}

func (s *Server) handleResize(w http.ResponseWriter, r *http.Request) {
	var req wire.ResizeRequest
	if !decodeRequest(w, r, &req) {
		return
	}
	session, err := s.session(req.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := session.Resize(r.Context(), req.Rows, req.Cols); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, struct{}{})
}

func (s *Server) handleSignal(w http.ResponseWriter, r *http.Request) {
	var req wire.SignalRequest
	if !decodeRequest(w, r, &req) {
		return
	}
	session, err := s.session(req.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := session.Signal(r.Context(), sandbox.SessionSignal(req.Signal)); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, struct{}{})
}

func (s *Server) handleTerminate(w http.ResponseWriter, r *http.Request) {
	var ref wire.SessionRef
	if !decodeRequest(w, r, &ref) {
		return
	}
	// The management path: Terminate syncs the runner's own record, so a List
	// that follows reflects the kill. The handle stays in the table because a
	// terminated session's output log is still readable, exactly as the
	// contract says.
	if err := s.runner.Terminate(r.Context(), ref.ID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, struct{}{})
}

func (s *Server) handleWait(w http.ResponseWriter, r *http.Request) {
	var req wire.WaitRequest
	if !decodeRequest(w, r, &req) {
		return
	}
	session, err := s.session(req.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.holdFor(req.HoldMS))
	defer cancel()

	exit, err := session.Wait(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && r.Context().Err() == nil {
			// Still running: the client re-asks. The exit is cached on the
			// server, so the retry costs one request, not one reaping.
			writeJSON(w, wire.WaitResponse{Running: true})
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, wire.WaitResponse{Exit: &exit})
}

func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	var ref wire.SessionRef
	if !decodeRequest(w, r, &ref) {
		return
	}
	session, err := s.session(ref.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := session.Close(); err != nil {
		writeError(w, err)
		return
	}
	s.forget(ref.ID)
	// Close reaps, so the exit is available now. Reporting it is what lets a
	// client answer Wait after Close the way the contract says it can.
	var exit *sandbox.SessionExit
	if got, err := session.Wait(r.Context()); err == nil {
		exit = &got
	}
	writeJSON(w, wire.CloseResponse{Exit: exit})
}

func (s *Server) handleJournal(w http.ResponseWriter, r *http.Request) {
	var req wire.JournalRequest
	if !decodeRequest(w, r, &req) {
		return
	}
	journal, err := s.journalReader(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	batch, err := journal.Read(r.Context(), req.AfterSeq, req.Max)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, batch)
}

// handleEvents streams one session's events as Server-Sent Events.
//
// The inner watcher already replays the retained output before delivering live
// events, so this is a forwarding loop: the resume cursor a consumer needs is the
// seq carried on each frame, and a consumer that fell behind recovers with a Read
// from the seq a lag event names — the contract's own recovery path, which is why
// this endpoint takes no cursor of its own.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	var req wire.EventsRequest
	if !decodeRequest(w, r, &req) {
		return
	}
	session, err := s.session(req.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, errdefs.NotAvailablef(
			"sandbox-remote: this ResponseWriter cannot flush, so it cannot stream"))
		return
	}

	watcher, err := session.Watch(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	defer func() { _ = watcher.Close() }()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// The one proxy directive that decides whether this is live or arrives in
	// batches at the end.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case ev, open := <-watcher.Events():
			if !open {
				return
			}
			if err := writeEvent(w, flusher, ev); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// writeEvent renders one frame. The id is the event's seq, so a reader holds its
// position from the frame itself.
func writeEvent(w http.ResponseWriter, flusher http.Flusher, ev sandbox.SessionEvent) error {
	frame := wire.FromEvent(ev)
	data, err := wire.Encode(frame)
	if err != nil {
		return err
	}
	// Assembled in one buffer and written once: a frame split across writes can
	// be flushed half-rendered.
	buf := make([]byte, 0, len(data)+48)
	buf = append(buf, "id: "...)
	buf = strconv.AppendInt(buf, ev.Seq, 10)
	buf = append(buf, "\nevent: "...)
	buf = append(buf, frame.Type...)
	buf = append(buf, "\ndata: "...)
	buf = append(buf, data...)
	buf = append(buf, '\n', '\n')
	if _, err := w.Write(buf); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// journalReader opens one reader for the lifetime of the server: FileJournal
// reads are cursor operations, so one reader serves every client cursor. It
// fails with whatever OpenJournal said, which is NotAvailable when the runner
// has no journal.
func (s *Server) journalReader(ctx context.Context) (sandbox.FileJournal, error) {
	s.journalOnce.Do(func() {
		s.journal, s.journalErr = sandbox.OpenJournal(ctx, s.runner)
	})
	if s.journalErr != nil {
		return nil, s.journalErr
	}
	return s.journal, nil
}

func (s *Server) holdFor(requestedMS int) time.Duration {
	if requestedMS <= 0 {
		return s.hold
	}
	return min(time.Duration(requestedMS)*time.Millisecond, s.hold)
}

func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		return false
	}
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return false
	}
	return subtleCompare(header[len(prefix):], s.token)
}

// subtleCompare is a constant-time comparison for the bearer token: a
// byte-by-byte early exit leaks the shared prefix.
func subtleCompare(got, want string) bool {
	if len(got) != len(want) {
		return false
	}
	var diff byte
	for i := range len(want) {
		diff |= got[i] ^ want[i]
	}
	return diff == 0
}

func (s *Server) register(session sandbox.Session) {
	s.mu.Lock()
	s.handles[session.ID()] = session
	s.mu.Unlock()
}

func (s *Server) forget(id string) {
	s.mu.Lock()
	delete(s.handles, id)
	s.mu.Unlock()
}

func (s *Server) session(id string) (sandbox.Session, error) {
	if id == "" {
		return nil, errdefs.Validationf("sandbox-remote: request names no session")
	}
	s.mu.Lock()
	session := s.handles[id]
	s.mu.Unlock()
	if session == nil {
		return nil, errdefs.NotFoundf(
			"sandbox-remote: session %q is not known to this server (Start or Attach it first)", id)
	}
	return session, nil
}

// sessionFor decodes a body that is only a session reference.
func (s *Server) sessionFor(w http.ResponseWriter, r *http.Request) (sandbox.Session, bool) {
	var ref wire.SessionRef
	if !decodeRequest(w, r, &ref) {
		return nil, false
	}
	session, err := s.session(ref.ID)
	if err != nil {
		writeError(w, err)
		return nil, false
	}
	return session, true
}

// readRequestBody reads one request body, reporting a read failure to the client.
func readRequestBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body := http.MaxBytesReader(w, r.Body, maxRequestBytes)
	defer func() { _ = body.Close() }()
	raw, err := io.ReadAll(body)
	if err != nil {
		writeError(w, errdefs.Validationf("sandbox-remote: read request: %w", err))
		return nil, false
	}
	return raw, true
}

func decodeRequest(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, ok := readRequestBody(w, r)
	if !ok {
		return false
	}
	if len(raw) == 0 {
		return true
	}
	if err := wire.Decode(raw, v); err != nil {
		writeError(w, errdefs.Validationf("sandbox-remote: malformed request: %w", err))
		return false
	}
	return true
}

// writeReadResponse writes one read result as the binary frame.
//
// The body is written piecewise — header, then each chunk's header and its bytes —
// so the server never assembles a second copy of the output. Content-Length is
// known up front from the frame size, which is what lets the client size its
// buffer once instead of growing it.
func writeReadResponse(w http.ResponseWriter, out sandbox.SessionOutput, idle bool) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(wire.ReadResponseSize(out, idle)))
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write(wire.AppendReadHeader(nil, out, idle)); err != nil {
		return
	}
	if idle {
		return
	}
	for _, chunk := range out.Chunks {
		if _, err := w.Write(wire.AppendChunkHeader(nil, chunk)); err != nil {
			return
		}
		if _, err := w.Write(chunk.Data); err != nil {
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	body, err := wire.Encode(v)
	if err != nil {
		writeError(w, errdefs.Internalf("sandbox-remote: encode response: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// writeError answers with the wire envelope and a status derived from the same
// classification the envelope carries, so a proxy, a log and the client agree.
func writeError(w http.ResponseWriter, err error) {
	envelope := wire.FromError(err)
	body, encErr := wire.Encode(envelope)
	if encErr != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	status := errdefs.HTTPStatus(err)
	if envelope.Code != "" {
		// The two plain sentinels are handle/cursor state, not policy: a
		// conflict is what a caller can act on (re-attach, re-read) and it is
		// what errdefs' own status table does not have a bucket for.
		status = http.StatusConflict
	}
	telemetry.WarnErr(context.Background(), "sandbox-remote: request failed", err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
