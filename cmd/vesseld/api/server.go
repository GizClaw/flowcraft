package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	otellog "go.opentelemetry.io/otel/log"

	"github.com/GizClaw/flowcraft/cmd/vesseld/fleet"
	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/vessel"
)

// Config bundles the bind parameters surface from the resolved
// DaemonPlan. The runtime layer translates DaemonPlan → Config so
// the api package does not depend on the resolver type.
type Config struct {
	// Socket is the unix socket path. Empty disables the unix
	// listener.
	Socket string

	// Listen is the TCP listen address. Empty disables TCP.
	Listen string

	// Token is the bearer token TCP requests must present in the
	// Authorization header. Required when Listen is non-empty;
	// the runtime layer reads the token-file and passes its
	// contents here.
	Token string

	// Version is the daemon version string returned by /v1/version.
	Version string
}

// Server is the HTTP control plane. Constructed once per daemon
// run; Start spawns the listeners and Stop tears them down with
// the supplied context's deadline as the graceful-close budget.
type Server struct {
	cfg   Config
	fleet *fleet.Fleet
	mux   *http.ServeMux

	// httpServer is shared between the unix and tcp listeners.
	// http.Server's Serve(net.Listener) lets us drive multiple
	// listeners off a single Server with one Shutdown call.
	httpServer *http.Server

	// ready flips to true once Launch returns. /readyz returns
	// 503 until then so kubernetes-style readiness probes do
	// not promote the daemon prematurely.
	ready atomic.Bool

	// listeners holds every accept loop so Stop can close them.
	listeners []net.Listener
}

// New builds a Server. Routes are wired here; listeners are
// created lazily by Start so tests can call Handler() directly.
func New(cfg Config, f *fleet.Fleet) *Server {
	s := &Server{cfg: cfg, fleet: f, mux: http.NewServeMux()}
	s.routes()
	s.httpServer = &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Handler returns the http.Handler so tests can drive routes via
// httptest.NewRecorder without spinning up a real listener.
func (s *Server) Handler() http.Handler { return s.mux }

// MarkReady flips /readyz to 200. Called by the runtime layer
// once Fleet.Launch returns successfully.
func (s *Server) MarkReady() { s.ready.Store(true) }

// Start binds every configured listener and serves until Stop is
// called. Returns the first listener-bind error.
func (s *Server) Start(ctx context.Context) error {
	if s.cfg.Socket == "" && s.cfg.Listen == "" {
		return fmt.Errorf("vesseld api: at least one of Socket / Listen must be set")
	}
	if s.cfg.Socket != "" {
		l, err := bindUnix(s.cfg.Socket)
		if err != nil {
			return err
		}
		s.listeners = append(s.listeners, l)
		go s.serveListener("unix", l)
	}
	if s.cfg.Listen != "" {
		if s.cfg.Token == "" {
			return fmt.Errorf("vesseld api: TCP listener requires a non-empty token")
		}
		l, err := net.Listen("tcp", s.cfg.Listen)
		if err != nil {
			return fmt.Errorf("vesseld api: bind tcp %s: %w", s.cfg.Listen, err)
		}
		s.listeners = append(s.listeners, l)
		go s.serveListener("tcp", l)
	}
	_ = ctx // ctx reserved for future startup-blocking probes
	return nil
}

// serveListener runs httpServer.Serve on l until Shutdown closes it.
// Surfaces non-Shutdown errors via telemetry — a listener that exits
// early (port stolen, fd exhaustion, kernel reset) would otherwise
// be invisible at the process level because the goroutine just exits.
func (s *Server) serveListener(kind string, l net.Listener) {
	if err := s.httpServer.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		telemetry.Warn(context.Background(), "vesseld api: listener exited",
			otellog.String("kind", kind),
			otellog.String("address", l.Addr().String()),
			otellog.String("error", err.Error()))
	}
}

// Stop shuts down the HTTP server gracefully. The supplied ctx
// deadline is the maximum time we wait for in-flight requests.
// Unix socket files are removed after Shutdown returns so a
// restart does not collide with a stale socket.
func (s *Server) Stop(ctx context.Context) error {
	err := s.httpServer.Shutdown(ctx)
	for _, l := range s.listeners {
		if u, ok := l.Addr().(*net.UnixAddr); ok {
			_ = os.Remove(u.Name)
		}
	}
	return err
}

// bindUnix listens on path with the parent directory created if
// missing and a stale socket file removed if present. We bind
// 0600 so only the daemon's user can connect — that is the auth
// boundary for the unix socket transport.
func bindUnix(path string) (net.Listener, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("vesseld api: mkdir %s: %w", dir, err)
		}
	}
	if _, err := os.Stat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("vesseld api: remove stale socket %s: %w", path, err)
		}
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("vesseld api: bind unix %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("vesseld api: chmod %s: %w", path, err)
	}
	return l, nil
}

// routes wires every endpoint. Order matters here in only one
// regard: /v1/vessels/{id}/... patterns are checked before the
// bare /v1/vessels list so the bare handler does not swallow them.
func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if !s.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready\n"))
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	// /metrics is intentionally unauthenticated: Prometheus
	// scrapers cannot easily present bearer tokens, and on the
	// unix socket the file mode is the auth boundary already.
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("GET /v1/version", s.authn(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": s.cfg.Version})
	}))
	s.mux.HandleFunc("GET /v1/vessels", s.authn(s.handleList))
	s.mux.HandleFunc("GET /v1/vessels/{id}/phase", s.authn(s.handlePhase))
	s.mux.HandleFunc("POST /v1/vessels/{id}/submit", s.authn(s.handleSubmit))
	s.mux.HandleFunc("POST /v1/vessels/{id}/call", s.authn(s.handleCall))
	s.mux.HandleFunc("POST /v1/vessels/{id}/resume", s.authn(s.handleResume))
	s.mux.HandleFunc("POST /v1/vessels/{id}/drain", s.authn(s.handleDrain))
	s.mux.HandleFunc("POST /v1/vessels/{id}/stop", s.authn(s.handleStop))
	s.mux.HandleFunc("GET /v1/vessels/{id}/logs", s.authn(s.handleLogs))
	s.mux.HandleFunc("GET /v1/runs", s.authn(s.handleRunList))
	s.mux.HandleFunc("GET /v1/runs/{run_id}", s.authn(s.handleRunStatus))
	s.mux.HandleFunc("GET /v1/plan", s.authn(s.handlePlan))
}

// handleRunStatus returns the terminal state of a previously
// submitted run. Closes the loop on /v1/vessels/{id}/submit which
// is fire-and-forget: callers receive a run_id, then poll this
// endpoint to discover status / messages / error after the run
// finishes. State is one of "running", "completed", "failed",
// "cancelled", or "error" (last when agent.Run itself errored).
//
// The registry retains entries for one hour; older runs return
// 404 with errdefs.NotFound. SSE log streaming via /logs remains
// the path for live progress.
func (s *Server) handleRunStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.fleet.LookupRun(r.PathValue("run_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleRunList renders the paged registry. Optional query params:
//
//	?vessel=<name>      filter by vessel name
//	?state=<state>      filter by terminal state (running|completed|...)
//	?page_size=N        clamp 1..500, default 50
//	?page_token=<tok>   opaque cursor returned by previous response
//
// The registry is in-memory only (see runRegistry doc for retention),
// so callers should not treat /v1/runs as a durable history feed —
// it is the operator's "what just happened" oracle, not the audit log.
func (s *Server) handleRunList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pageSize := 0
	if raw := q.Get("page_size"); raw != "" {
		if n, err := strconvAtoi(raw); err == nil {
			pageSize = n
		}
	}
	page := s.fleet.ListRuns(fleet.ListRunsOptions{
		Vessel:    q.Get("vessel"),
		State:     q.Get("state"),
		PageSize:  pageSize,
		PageToken: q.Get("page_token"),
	})
	writeJSON(w, http.StatusOK, page)
}

// strconvAtoi is a tiny wrapper that returns (n, err); kept here
// instead of importing strconv at the package level just for one
// callsite would require shuffling unrelated imports. The api
// package already pulls in strings / fmt; pulling in strconv as
// well is a no-op at runtime cost.
func strconvAtoi(s string) (int, error) {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int(ch-'0')
	}
	return n, nil
}

// authn enforces the Authorization: Bearer <token> contract for
// TCP requests. Unix socket requests are detected via RemoteAddr
// containing "@" / unix prefix and skip the check (filesystem
// permissions are the auth boundary there).
func (s *Server) authn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if isUnixRequest(r) || s.cfg.Token == "" {
			next(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) || subtleEqual(auth[len(prefix):], s.cfg.Token) != true {
			writeError(w, errdefs.NotAvailablef("vesseld api: authentication required"))
			return
		}
		next(w, r)
	}
}

// isUnixRequest sniffs the connection address so the auth filter
// can skip TCP-only checks for unix socket clients. http.Server
// reports unix listeners via the @ prefix on the RemoteAddr.
func isUnixRequest(r *http.Request) bool {
	return strings.HasPrefix(r.RemoteAddr, "@") || strings.Contains(r.RemoteAddr, ".sock")
}

// subtleEqual is a constant-time string compare so token
// validation does not leak length / prefix info via timing.
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// handleList returns every vessel name plus its current phase.
func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	type entry struct {
		Name  string `json:"name"`
		Phase string `json:"phase"`
	}
	out := make([]entry, 0, len(s.fleet.Names()))
	for _, name := range s.fleet.Names() {
		c, err := s.fleet.Captain(name)
		if err != nil {
			continue
		}
		out = append(out, entry{Name: name, Phase: string(c.Phase())})
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePhase returns the current phase for a single vessel.
func (s *Server) handlePhase(w http.ResponseWriter, r *http.Request) {
	c, err := s.fleet.Captain(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"phase": string(c.Phase())})
}

// submitBody is the wire form for POST /submit and /call.
type submitBody struct {
	AgentName string         `json:"agent"`
	Query     string         `json:"query"`
	ContextID string         `json:"context_id,omitempty"`
	Inputs    map[string]any `json:"inputs,omitempty"`
}

// handleSubmit dispatches and returns the run id immediately.
//
// Important: we MUST NOT pass r.Context() to fleet.Submit here.
// net/http cancels the request context the moment the handler
// returns, which would tear the dispatched Run down before the
// LLM ever gets called. The whole point of /submit (vs /call)
// is fire-and-forget — so we hand the daemon a long-lived
// background context and leave per-run cancellation to /stop /
// /drain endpoints.
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	body, err := decodeSubmit(r)
	if err != nil {
		writeError(w, err)
		return
	}
	h, err := s.fleet.Submit(context.Background(), r.PathValue("id"), body.AgentName, requestFrom(body))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"run_id": h.RunID,
	})
}

// handleCall is the synchronous variant: dispatches and waits.
func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	body, err := decodeSubmit(r)
	if err != nil {
		writeError(w, err)
		return
	}
	h, err := s.fleet.Submit(r.Context(), r.PathValue("id"), body.AgentName, requestFrom(body))
	if err != nil {
		writeError(w, err)
		return
	}
	res, err := h.Wait(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	out := map[string]any{
		"run_id":   h.RunID,
		"status":   string(res.Status),
		"messages": res.Messages,
	}
	// Surface the underlying error message on non-completed runs
	// so HTTP callers do not have to dig in stderr to figure out
	// what failed. The Go-side error stays unstructured here
	// (no errdefs reason mapping) because /call results are run-
	// level, not request-level: a "failed" status with err is
	// still a successful HTTP request.
	if res.Err != nil {
		out["error"] = res.Err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// resumeBody is the wire form for POST /resume.
type resumeBody struct {
	RunID string `json:"run_id"`
}

// handleResume re-launches a previously interrupted run via
// [vessel.Captain.Resume]. Body shape:
//
//	{ "run_id": "..." }
//
// On success returns 202 Accepted with {"run_id": "..."} so the
// caller can poll /v1/runs/{run_id} for terminal state — same
// contract as /submit.
//
// Error classes are propagated from the underlying Captain.Resume
// (NotAvailable when no checkpoint store, NotFound when the
// checkpoint or its agent are missing, Validation when run_id is
// empty); writeError maps them to HTTP status codes.
//
// Like /submit we use context.Background() for the dispatch so the
// HTTP request lifecycle does not cancel the resumed run mid-flight.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	var body resumeBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, errdefs.Validationf("vesseld api: decode body: %v", err))
		return
	}
	if body.RunID == "" {
		writeError(w, errdefs.Validationf("vesseld api: body.run_id is required"))
		return
	}
	h, err := s.fleet.Resume(context.Background(), r.PathValue("id"), body.RunID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"run_id": h.RunID})
}

// handleDrain triggers Captain.Drain. Honours the request context
// deadline as the drain budget; clients wanting a different budget
// should set HTTP request timeout accordingly.
func (s *Server) handleDrain(w http.ResponseWriter, r *http.Request) {
	c, err := s.fleet.Captain(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if err := c.Drain(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"phase": string(c.Phase())})
}

// handleStop hard-stops a single Captain. The fleet keeps the
// entry so subsequent Submits surface NotAvailable instead of the
// vessel disappearing entirely.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	c, err := s.fleet.Captain(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if err := c.Stop(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"phase": string(c.Phase())})
}

// handleLogs streams stream-delta envelopes via SSE. Each event
// is a single JSON object with run_id + delta fields. The stream
// closes when the client disconnects or the captain bus closes.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	c, err := s.fleet.Captain(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, errdefs.NotAvailablef("vesseld api: response writer does not support flushing"))
		return
	}

	// Subscribe BEFORE writing 200. The previous order let the
	// client observe the 200 and start submitting work while the
	// daemon was still wiring its bus subscription, which made
	// "subscribe → submit → see start event" e2e tests flaky.
	// Doing it in this order means by the time the client sees
	// the response headers, the subscription is guaranteed to be
	// receiving envelopes.
	runID := r.URL.Query().Get("run_id")
	var ch <-chan vessel.LogEntry
	if runID != "" {
		ch, err = vessel.LogsForRun(r.Context(), c, runID)
	} else {
		ch, err = vessel.Logs(r.Context(), c)
	}
	if err != nil {
		writeError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// SSE wire format: each LogEntry is emitted as a typed event.
	//
	//	event: <LogEntry.Type>
	//	data:  <JSON of LogEntry>
	//	<blank line>
	//
	// The "event:" line lets browser EventSource consumers register
	// per-type listeners (e.g. addEventListener("run.ended", ...))
	// without parsing the JSON body. The "data:" body still carries
	// the full LogEntry so non-EventSource clients (raw HTTP
	// readers, the e2e helper) get the same information either way.
	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case entry, ok := <-ch:
			if !ok {
				return
			}
			if entry.Type != "" {
				_, _ = w.Write([]byte("event: "))
				_, _ = w.Write([]byte(entry.Type))
				_, _ = w.Write([]byte("\n"))
			}
			_, _ = w.Write([]byte("data: "))
			_ = enc.Encode(entry) // writes the JSON body + "\n"
			// SSE event terminator: a blank line. Without this
			// every event blends into the next one and consumers
			// (browser EventSource, helpers' bufio.Scanner)
			// never dispatch any of them.
			_, _ = w.Write([]byte("\n"))
			flusher.Flush()
		}
	}
}

// handlePlan returns the resolved Plan with secrets redacted.
// The Plan struct is not directly JSON-friendly (closures, maps
// of factories) so we hand-roll a small projection here.
//
// The projection covers the static information operators need to
// confirm "did the daemon actually parse my YAML correctly?":
// every vessel with its declared agents (name, engine ref, dispatcher
// flag, sidecar flag, history access mode), the history-store name,
// the live phase, and the daemon-wide drain timeout. Closures (engine
// factories, probe instances) are intentionally NOT rendered.
func (s *Server) handlePlan(w http.ResponseWriter, _ *http.Request) {
	type agentSummary struct {
		Name          string `json:"name"`
		EngineRef     string `json:"engine"`
		Dispatcher    bool   `json:"dispatcher,omitempty"`
		Sidecar       bool   `json:"sidecar,omitempty"`
		HistoryAccess string `json:"history_access,omitempty"`
	}
	type vesselSummary struct {
		Name    string         `json:"name"`
		Phase   string         `json:"phase"`
		History string         `json:"history,omitempty"`
		Agents  []agentSummary `json:"agents"`
	}
	dp := s.fleet.DaemonPlan()
	out := struct {
		Daemon       string          `json:"daemon"`
		Version      string          `json:"version"`
		DrainTimeout string          `json:"drain_timeout,omitempty"`
		Vessels      []vesselSummary `json:"vessels"`
	}{
		Daemon:  dp.Name,
		Version: s.cfg.Version,
	}
	if dp.DrainTimeout > 0 {
		out.DrainTimeout = dp.DrainTimeout.String()
	}
	for _, name := range s.fleet.Names() {
		cap, err := s.fleet.Captain(name)
		if err != nil {
			continue
		}
		v := vesselSummary{Name: name, Phase: string(cap.Phase())}
		if vp, ok := s.fleet.VesselPlan(name); ok {
			v.History = vp.HistoryName
			for _, a := range vp.Spec.Agents {
				v.Agents = append(v.Agents, agentSummary{
					Name:          a.Name,
					EngineRef:     vp.EngineRefByAgent[a.Name],
					Dispatcher:    a.Dispatcher,
					Sidecar:       a.Sidecar,
					HistoryAccess: string(a.HistoryAccess),
				})
			}
		}
		out.Vessels = append(out.Vessels, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// decodeSubmit parses the submit/call body and validates required
// fields. agent name MUST be set; everything else is optional.
func decodeSubmit(r *http.Request) (submitBody, error) {
	var body submitBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return body, errdefs.Validationf("vesseld api: decode body: %v", err)
	}
	if body.AgentName == "" {
		return body, errdefs.Validationf("vesseld api: body.agent is required")
	}
	return body, nil
}

// requestFrom translates the wire body into an agent.Request the
// vessel runtime accepts. The body's "query" string surfaces as
// the user-role Message; structured inputs flow through as-is.
func requestFrom(b submitBody) agent.Request {
	return agent.Request{
		ContextID: b.ContextID,
		Message:   model.NewTextMessage(model.RoleUser, b.Query),
		Inputs:    b.Inputs,
	}
}
