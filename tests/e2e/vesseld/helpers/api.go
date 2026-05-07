package helpers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// PostJSON issues an authenticated POST against the daemon. body
// is JSON-encoded; pass nil for an empty body. The auth header is
// set to "Bearer e2e-token" matching the default config templates;
// override with WithAuthToken if the test uses something else.
//
// Returned response body is fully read into memory before return
// so callers do not need to defer Close — the function takes care
// of it. The status code lives in resp.StatusCode and the parsed
// body lives in `out` when non-nil; pass `out=nil` to discard.
func (h *DaemonHandle) PostJSON(t *testing.T, path string, body, out any, opts ...ReqOpt) *http.Response {
	t.Helper()
	return h.doJSON(t, http.MethodPost, path, body, out, opts...)
}

// GetJSON issues an authenticated GET; otherwise identical to
// PostJSON. body is always nil and is accepted only for symmetry.
func (h *DaemonHandle) GetJSON(t *testing.T, path string, out any, opts ...ReqOpt) *http.Response {
	t.Helper()
	return h.doJSON(t, http.MethodGet, path, nil, out, opts...)
}

// Do is the low-level escape hatch when a test needs full control
// of headers / status / streaming. It dials the daemon's unix
// socket, sets the default auth header, and returns the raw
// response — caller must Close.
func (h *DaemonHandle) Do(t *testing.T, method, path string, body io.Reader, opts ...ReqOpt) *http.Response {
	t.Helper()
	cfg := buildReqCfg(opts)
	url := "http://vesseld" + path
	req, err := http.NewRequestWithContext(cfg.ctx, method, url, body)
	if err != nil {
		t.Fatalf("e2e: build request: %v", err)
	}
	if cfg.auth != "" {
		req.Header.Set("Authorization", cfg.auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range cfg.headers {
		req.Header.Set(k, v)
	}
	resp, err := h.HTTPClient().Do(req)
	if err != nil {
		t.Fatalf("e2e: %s %s: %v", method, path, err)
	}
	return resp
}

func (h *DaemonHandle) doJSON(t *testing.T, method, path string, body, out any, opts ...ReqOpt) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("e2e: encode body: %v", err)
		}
		rdr = bytes.NewReader(buf)
	}
	resp := h.Do(t, method, path, rdr, opts...)
	defer resp.Body.Close()
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("e2e: decode response: %v", err)
		}
	} else {
		// drain so the connection can be reused
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return resp
}

// ReqOpt customises an outbound request.
type ReqOpt func(*reqCfg)

type reqCfg struct {
	ctx     context.Context
	auth    string
	headers map[string]string
}

func buildReqCfg(opts []ReqOpt) reqCfg {
	c := reqCfg{
		ctx: context.Background(),
	}
	for _, opt := range opts {
		opt(&c)
	}
	return c
}

// WithCtx attaches a context to the outbound request.
func WithCtx(ctx context.Context) ReqOpt { return func(c *reqCfg) { c.ctx = ctx } }

// WithAuthToken overrides the default Bearer token. Pass empty
// string to send no Authorization header at all.
func WithAuthToken(tok string) ReqOpt {
	return func(c *reqCfg) {
		if tok == "" {
			c.auth = ""
			return
		}
		c.auth = "Bearer " + tok
	}
}

// WithRawAuth sends the literal Authorization value (no "Bearer"
// prefix added). Useful for negative tests like "lowercase bearer
// is rejected".
func WithRawAuth(v string) ReqOpt { return func(c *reqCfg) { c.auth = v } }

// WithHeader adds (or overrides) a single request header.
func WithHeader(k, v string) ReqOpt {
	return func(c *reqCfg) {
		if c.headers == nil {
			c.headers = map[string]string{}
		}
		c.headers[k] = v
	}
}

// Submit is a convenience wrapper around POST /v1/vessels/{id}/submit
// that returns the run_id for a successful 202 response, or fails
// the test on any non-202 outcome.
func (h *DaemonHandle) Submit(t *testing.T, vessel, agent, query string, inputs map[string]any) string {
	t.Helper()
	body := map[string]any{"agent": agent, "query": query}
	if inputs != nil {
		body["inputs"] = inputs
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	resp := h.PostJSON(t, "/v1/vessels/"+url.PathEscape(vessel)+"/submit", body, &out)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit %s/%s: status=%d, want 202", vessel, agent, resp.StatusCode)
	}
	if out.RunID == "" {
		t.Fatal("submit returned empty run_id")
	}
	return out.RunID
}

// WaitRun polls GET /v1/runs/{run_id} until the run reaches a
// terminal state (status != "running") or the budget elapses. The
// returned map is the raw decoded JSON; callers assert on
// "status", "error", "error_code", etc.
func (h *DaemonHandle) WaitRun(t *testing.T, runID string, budget time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		var out map[string]any
		resp := h.GetJSON(t, "/v1/runs/"+url.PathEscape(runID), &out)
		if resp.StatusCode == http.StatusOK {
			if state, _ := out["state"].(string); state != "" && state != "running" && state != "pending" {
				return out
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("e2e: run %s did not reach terminal state within %s\nstderr:\n%s", runID, budget, h.Stderr())
	return nil
}

// LogsStream subscribes to /v1/vessels/{id}/logs (SSE) and returns
// a channel of decoded JSON envelopes plus a cancel func. The
// channel is closed when the connection terminates (server side
// disconnect, ctx cancel, or test done). Tests use this to assert
// "the daemon emitted event X".
type LogEvent struct {
	Event string         // SSE "event:" name (default "message")
	Data  map[string]any // parsed JSON from the "data:" line
	Raw   string         // unparsed data line for ad-hoc inspection
}

func (h *DaemonHandle) LogsStream(t *testing.T, ctx context.Context, vessel string, runID string) (<-chan LogEvent, func()) {
	t.Helper()
	path := "/v1/vessels/" + url.PathEscape(vessel) + "/logs"
	if runID != "" {
		path += "?run_id=" + url.QueryEscape(runID)
	}
	subCtx, cancel := context.WithCancel(ctx)
	resp := h.Do(t, http.MethodGet, path, nil, WithCtx(subCtx))
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		t.Fatalf("logs stream status=%d", resp.StatusCode)
	}

	out := make(chan LogEvent, 32)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var ev LogEvent
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				ev.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				ev.Raw = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				_ = json.Unmarshal([]byte(ev.Raw), &ev.Data)
			case line == "":
				if ev.Raw != "" || ev.Event != "" {
					select {
					case out <- ev:
					case <-subCtx.Done():
						return
					}
				}
				ev = LogEvent{}
			}
		}
	}()
	return out, cancel
}

// WaitForLog drains the LogsStream channel until it sees an event
// whose Event == eventType (and runID matches when non-empty), or
// until budget elapses. Returns the matching event or nil on timeout.
//
// Designed for the conformance suite: instead of inlining a
// for-select-deadline loop in every "wait for run.ended" test, the
// helper bakes in the cancel-on-budget semantics so the caller
// reads as a single intent line.
//
// Drain (non-matching) events are returned alongside as the second
// value so consumers asserting full-stream invariants (e.g. seq
// monotonicity) can keep them.
func WaitForLog(t *testing.T, ch <-chan LogEvent, eventType string, runID string, budget time.Duration) (matched *LogEvent, drained []LogEvent) {
	t.Helper()
	deadline := time.After(budget)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return nil, drained
			}
			drained = append(drained, ev)
			if ev.Event != eventType {
				continue
			}
			if runID != "" {
				if got, _ := ev.Data["run_id"].(string); got != runID {
					continue
				}
			}
			matched = &drained[len(drained)-1]
			return matched, drained
		case <-deadline:
			return nil, drained
		}
	}
}

// GrepStderr returns true when the daemon's captured stderr
// contains the substring s. Useful for "we logged a warn line"
// assertions where the structured event is hard to subscribe to
// (e.g. probe / kanban diagnostics).
func (h *DaemonHandle) GrepStderr(s string) bool {
	return strings.Contains(h.Stderr(), s)
}

// WaitStderr polls GrepStderr until it matches or budget elapses.
func (h *DaemonHandle) WaitStderr(t *testing.T, s string, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if h.GrepStderr(s) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("stderr never contained %q within %s\nstderr:\n%s", s, budget, h.Stderr())
}

// Phase fetches GET /v1/vessels/{id}/phase and returns the parsed
// phase string. Test-friendly wrapper.
func (h *DaemonHandle) Phase(t *testing.T, vessel string) string {
	t.Helper()
	var out struct {
		Phase string `json:"phase"`
	}
	resp := h.GetJSON(t, "/v1/vessels/"+url.PathEscape(vessel)+"/phase", &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("phase %s status=%d", vessel, resp.StatusCode)
	}
	return out.Phase
}

// WaitPhase polls Phase until it equals one of `want` or budget
// elapses.
func (h *DaemonHandle) WaitPhase(t *testing.T, vessel string, budget time.Duration, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		got := h.Phase(t, vessel)
		for _, w := range want {
			if got == w {
				return got
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("vessel %s never reached phase %v within %s (last seen=%q)", vessel, want, budget, h.Phase(t, vessel))
	return ""
}

// Plan fetches /v1/plan and returns the parsed map. Tests assert
// on the projected daemon/vessel/agent shape.
func (h *DaemonHandle) Plan(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	resp := h.GetJSON(t, "/v1/plan", &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("plan status=%d", resp.StatusCode)
	}
	return out
}

// MustHTTP panics-via-t.Fatal when status does not match.
// Kept here for the many tests that do "GET /healthz expecting
// 200" without caring about the body.
func (h *DaemonHandle) MustHTTP(t *testing.T, method, path string, want int, opts ...ReqOpt) {
	t.Helper()
	resp := h.Do(t, method, path, nil, opts...)
	defer resp.Body.Close()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s: status=%d, want %d\nbody: %s", method, path, resp.StatusCode, want, string(body))
	}
}

// Pid returns the daemon subprocess PID. Useful for OS-level
// asserts (e.g. "no goroutine leaks: kill the process and inspect
// /proc/<pid> existence").
func (h *DaemonHandle) Pid() int {
	if h.cmd == nil || h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
}

// SignalQuit sends SIGTERM but does NOT wait. Use when the test
// wants to inject a signal mid-run and observe what happens to
// in-flight requests.
func (h *DaemonHandle) SignalQuit() error {
	if h.cmd == nil || h.cmd.Process == nil {
		return fmt.Errorf("daemon not running")
	}
	return h.cmd.Process.Signal(interruptSignal)
}

// WaitExit blocks until the daemon process exits, returning the
// captured exit error (nil on clean exit). Returns os.ErrDeadlineExceeded
// when budget elapses.
func (h *DaemonHandle) WaitExit(budget time.Duration) error {
	select {
	case <-h.exited:
		return h.exitErr
	case <-time.After(budget):
		return fmt.Errorf("daemon did not exit within %s", budget)
	}
}
