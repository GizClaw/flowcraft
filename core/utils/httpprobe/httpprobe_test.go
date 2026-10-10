package httpprobe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/utils"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"
)

// logSink records the telemetry the probe emits, so a test can pin the
// two boundaries, their attributes, and their severities rather than
// only that something was logged.
type logSink struct {
	mu      sync.Mutex
	records []logRecord
}

type logRecord struct {
	body     string
	attrs    map[string]string
	severity string
}

func (s *logSink) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }

func (s *logSink) OnEmit(_ context.Context, record *sdklog.Record) error {
	clone := record.Clone()
	attrs := make(map[string]string, clone.AttributesLen())
	clone.WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = valueString(kv.Value)
		return true
	})
	s.mu.Lock()
	s.records = append(s.records, logRecord{
		body:     clone.Body().AsString(),
		attrs:    attrs,
		severity: clone.Severity().String(),
	})
	s.mu.Unlock()
	return nil
}

func (s *logSink) Shutdown(context.Context) error   { return nil }
func (s *logSink) ForceFlush(context.Context) error { return nil }

// find returns every recorded line with the given body.
func (s *logSink) find(body string) []logRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []logRecord
	for _, r := range s.records {
		if r.body == body {
			out = append(out, r)
		}
	}
	return out
}

// len returns the number of records so far.
func (s *logSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// installLogSink points the process-wide logger at a recording sink.
func installLogSink(t *testing.T) *logSink {
	t.Helper()
	sink := &logSink{}
	shutdown, err := telemetry.InitLog(context.Background(),
		telemetry.WithLogProcessor(sink))
	if err != nil {
		t.Fatalf("init log: %v", err)
	}
	telemetry.Enable()
	t.Cleanup(func() {
		_ = shutdown(context.Background())
	})
	return sink
}

// valueString renders one attribute value the way the log line's
// reader sees it.
func valueString(v attribute.Value) string {
	switch v.Type() {
	case attribute.STRING:
		return v.AsString()
	case attribute.BOOL:
		return strconv.FormatBool(v.AsBool())
	case attribute.INT64:
		return strconv.FormatInt(v.AsInt64(), 10)
	default:
		return v.String()
	}
}

// roundTripFunc adapts a function to a RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okResponse() *http.Response {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		ContentLength: 2,
		Body:          io.NopCloser(strings.NewReader("{}")),
	}
}

func probeRequest(method, url, body string) *http.Request {
	return httptest.NewRequest(method, url, strings.NewReader(body))
}

func TestProbeRecordsBothBoundaries(t *testing.T) {
	sink := installLogSink(t)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3, 4},
		SpanID:     trace.SpanID{9, 9},
		TraceFlags: trace.FlagsSampled,
	})
	req := probeRequest(http.MethodPost, "https://api.example.com/v1/chat/completions",
		`{"model":"m"}`).WithContext(trace.ContextWithSpanContext(context.Background(), sc))

	probe := New(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return okResponse(), nil
	}))
	resp, err := probe.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	_ = resp.Body.Close()

	dispatched := sink.find("httpprobe: request dispatched")
	received := sink.find("httpprobe: response headers received")
	if len(dispatched) != 1 || len(received) != 1 {
		t.Fatalf("records: dispatched=%d received=%d, want one each",
			len(dispatched), len(received))
	}
	want := map[string]string{
		"http.method":   "POST",
		"http.host":     "api.example.com",
		"http.path":     "/v1/chat/completions",
		"request_bytes": "13",
		"trace_id":      sc.TraceID().String(),
	}
	for key, value := range want {
		if got := dispatched[0].attrs[key]; got != value {
			t.Errorf("dispatched %s = %q, want %q", key, got, value)
		}
	}
	if got := received[0].attrs["http.status"]; got != "200" {
		t.Errorf("response status attr = %q, want 200", got)
	}
	if _, ok := received[0].attrs["wait_ms"]; !ok {
		t.Errorf("response record has no wait_ms: %v", received[0].attrs)
	}
	if got := received[0].attrs["content_type"]; got != "application/json" {
		t.Errorf("content_type = %q", got)
	}
	if got := received[0].attrs["content_length_known"]; got != "true" {
		t.Errorf("content_length_known = %q, want true", got)
	}
}

func TestProbeSkipsUntargetedTraffic(t *testing.T) {
	sink := installLogSink(t)
	probe := New(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return okResponse(), nil
	}))
	for _, tc := range []struct {
		method string
		url    string
		want   int
	}{
		{http.MethodGet, "https://api.example.com/v1/models", 0},
		{http.MethodPost, "https://collector.example.com/v1/logs", 0},
		{http.MethodPost, "https://collector.example.com/v1/traces", 0},
		{http.MethodPost, "https://collector.example.com/v1/metrics", 0},
		{http.MethodDelete, "https://api.example.com/v1/files/f1", 0},
		{http.MethodPost, "https://api.example.com/v1/chat/completions", 2},
	} {
		before := sink.len()
		resp, err := probe.RoundTrip(probeRequest(tc.method, tc.url, "{}"))
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.url, err)
		}
		_ = resp.Body.Close()
		if got := sink.len() - before; got != tc.want {
			t.Errorf("%s %s recorded %d lines, want %d", tc.method, tc.url, got, tc.want)
		}
	}
}

// stubTransport is an addressable base transport for the global-form
// test: a pointer type so the restored default can be compared.
type stubTransport struct{ calls atomic.Int64 }

func (s *stubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	s.calls.Add(1)
	return okResponse(), nil
}

func TestInstallIsIdempotentAndUninstallSilences(t *testing.T) {
	sink := installLogSink(t)
	preserve := http.DefaultTransport
	stub := &stubTransport{}
	t.Cleanup(func() { http.DefaultTransport = preserve })
	http.DefaultTransport = stub

	if !Install() {
		t.Fatal("Install must report the probe is in place")
	}
	wrapped, ok := http.DefaultTransport.(*Probe)
	if !ok {
		t.Fatalf("DefaultTransport = %T, want the probe", http.DefaultTransport)
	}
	if !Install() || !Active() {
		t.Fatal("a second Install must be a no-op that reports active")
	}
	if http.DefaultTransport != wrapped {
		t.Fatal("a second Install must not wrap again")
	}
	if !Uninstall() {
		t.Fatal("Uninstall must report the wrapper it removed")
	}
	if Active() {
		t.Fatal("Uninstall must clear the installed state")
	}
	if http.DefaultTransport != stub {
		t.Fatalf("Uninstall must restore the wrapped transport, got %T", http.DefaultTransport)
	}
	if Uninstall() {
		t.Fatal("a second Uninstall must report nothing to do")
	}

	// A client that captured the wrapper keeps transporting but goes
	// quiet after the uninstall.
	resp, err := wrapped.RoundTrip(probeRequest(http.MethodPost,
		"https://api.example.com/v1/chat/completions", "{}"))
	if err != nil {
		t.Fatalf("a handed-out wrapper must keep transporting: %v", err)
	}
	_ = resp.Body.Close()
	if stub.calls.Load() != 1 {
		t.Fatalf("base calls = %d, want the request to have gone through", stub.calls.Load())
	}
	if got := sink.len(); got != 0 {
		t.Fatalf("records after uninstall = %d, want none", got)
	}
}

func TestProbeRecordsFailureAndReturnsIt(t *testing.T) {
	sink := installLogSink(t)
	wantErr := errors.New("dial tcp: connection refused")
	probe := New(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, wantErr
	}))
	if _, err := probe.RoundTrip(probeRequest(http.MethodPost,
		"https://api.example.com/v1/chat/completions", "{}")); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the transport error through unchanged", err)
	}
	failed := sink.find("httpprobe: request failed")
	if len(failed) != 1 {
		t.Fatalf("failure records = %d, want 1", len(failed))
	}
	if !strings.Contains(failed[0].severity, "WARN") {
		t.Fatalf("failure severity = %q, want WARN", failed[0].severity)
	}
	if got := sink.find("httpprobe: response headers received"); len(got) != 0 {
		t.Fatalf("a failed round trip must not record a response boundary: %v", got)
	}
}

func TestEnabledReadsEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"true", true},
		{"1", true},
		{"false", false},
		{"0", false},
		{"", false},
		{"not-a-bool", false},
	} {
		t.Setenv(Env, tc.value)
		if got := Enabled(); got != tc.want {
			t.Errorf("Enabled() with %q = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// TestWrappedDefaultTransportDoesNotBreakHTTPKit pins the regression the
// package comment warns about: core/utils builds its transports by
// cloning http.DefaultTransport through a *http.Transport assertion, so
// a wrapped process transport (the probe installed) must fall back to a
// fresh transport instead of panicking the MCP and extractor clients
// that call it during a reload.
func TestWrappedDefaultTransportDoesNotBreakHTTPKit(t *testing.T) {
	preserve := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = preserve })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(server.Close)

	http.DefaultTransport = New(preserve)
	transport := utils.NewRoundTripper(utils.WithoutRetry())
	resp, err := (&http.Client{Transport: transport}).Get(server.URL)
	if err != nil {
		t.Fatalf("round trip through the fallback transport: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want ok", body)
	}
}
