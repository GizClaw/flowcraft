package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAPI_Metrics_TextExposition asserts that /metrics renders the
// Prometheus text-exposition format with the minimum metric set
// pinned by the api package contract (build_info, runs_inflight,
// runs_total, run_duration_seconds_*, vessel_phase). Without this
// any later refactor that
// silently drops a series goes unnoticed by docs / dashboards.
func TestAPI_Metrics_TextExposition(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)

	// Drive one round-trip so the run-totals counter has at least
	// one series — keeps the test honest about the counter format.
	// vessel.Handle.OnTerminate fires synchronously before /call
	// returns, so the registry's terminal counter is guaranteed
	// to be populated by the time we scrape /metrics. No polling
	// needed — if this test ever flakes again it means the
	// OnTerminate ordering contract has regressed.
	_ = httpServerCall(t, s, http.MethodPost, "/v1/vessels/support/call",
		strings.NewReader(`{"agent":"helper","query":"hi"}`))

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	got := w.Body.String()

	// Content-type must be the standard exposition format, else
	// some scraper implementations refuse the payload.
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type=%q want text/plain*", ct)
	}

	mustContain := []string{
		`# TYPE vesseld_build_info gauge`,
		`vesseld_build_info{version="test"} 1`,
		`# TYPE vesseld_runs_inflight gauge`,
		`vesseld_runs_inflight{vessel="support"}`,
		`# TYPE vesseld_runs_total counter`,
		`vesseld_runs_total{vessel="support",state="completed"}`,
		`# TYPE vesseld_run_duration_seconds_sum counter`,
		`vesseld_run_duration_seconds_count{vessel="support"}`,
		`# TYPE vesseld_vessel_phase gauge`,
		`vesseld_vessel_phase{vessel="support",phase="running"} 1`,
		`vesseld_vessel_phase{vessel="support",phase="failed"} 0`,
	}
	for _, want := range mustContain {
		if !strings.Contains(got, want) {
			t.Errorf("missing line %q in /metrics output:\n%s", want, got)
		}
	}
}

// TestAPI_Metrics_NoAuthRequired confirms /metrics is reachable
// even when a TCP bearer token is configured. Prometheus scrapers
// can't easily present bearer tokens; locking down /metrics would
// make every k8s-style deployment unscrapable.
func TestAPI_Metrics_NoAuthRequired(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	s.cfg.Token = "secret"
	s.cfg.Listen = ":0"

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "10.0.0.1:1234" // simulate a TCP request
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

// httpServerCall is a tiny helper that routes a request through the
// server and asserts non-error status; mirrors the implicit pattern
// already used in TestAPI_Call_RoundTrip.
func httpServerCall(t *testing.T, s *Server, method, target string, body *strings.Reader) string {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(method, target, body))
	if w.Code/100 != 2 {
		t.Fatalf("call %s %s -> %d body=%s", method, target, w.Code, w.Body.String())
	}
	return w.Body.String()
}
