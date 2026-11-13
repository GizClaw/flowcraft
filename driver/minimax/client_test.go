package minimax

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

// errTest is the cause the classification helpers wrap, so each case exercises
// the category marker rather than the message text.
var errTest = errors.New("boom")

func TestRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "rate limit", err: errdefs.RateLimit(errTest), want: true},
		{name: "unavailable", err: errdefs.NotAvailable(errTest), want: true},
		{name: "timeout", err: errdefs.Timeout(errTest), want: true},
		{name: "internal", err: errdefs.Internal(errTest), want: true},
		{name: "validation", err: errdefs.Validation(errTest), want: false},
		{name: "unauthorized", err: errdefs.Unauthorized(errTest), want: false},
		{name: "forbidden", err: errdefs.Forbidden(errTest), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryable(tc.err); got != tc.want {
				t.Fatalf("retryable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRetryWaitPrefersServerHint(t *testing.T) {
	hinted := errdefs.WithRetryAfter(errdefs.RateLimit(errTest), 2*time.Second)
	if got := retryWait(hinted, 0); got != 2*time.Second {
		t.Fatalf("wait = %v, want the server hint", got)
	}
	capped := errdefs.WithRetryAfter(errdefs.RateLimit(errTest), time.Hour)
	if got := retryWait(capped, 0); got != retryAfterMax {
		t.Fatalf("wait = %v, want it capped at %v", got, retryAfterMax)
	}
	backoff := retryWait(errdefs.RateLimit(errTest), 0)
	if backoff != retryBackoffBase {
		t.Fatalf("wait = %v, want %v", backoff, retryBackoffBase)
	}
	if later := retryWait(errdefs.RateLimit(errTest), 1); later != 2*retryBackoffBase {
		t.Fatalf("wait = %v, want the doubled backoff", later)
	}
	if capped := retryWait(errdefs.RateLimit(errTest), 20); capped != retryBackoffMax {
		t.Fatalf("wait = %v, want %v", capped, retryBackoffMax)
	}
}

// TestReadRequestsRetryTransientFailures pins the half of the retry budget that
// is safe: the video task poll is a read, so a transient failure is retried.
func TestReadRequestsRetryTransientFailures(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want a read", r.Method)
		}
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"task": map[string]any{"status": "succeeded"}})
	}))
	defer server.Close()

	client := newMediaClient(Settings{BaseURL: server.URL, APIKey: "key", Retries: 1})
	var out struct {
		Task struct {
			Status string `json:"status"`
		} `json:"task"`
	}
	if err := client.getJSON(context.Background(), "/v2/query/video_generation/t1", &out); err != nil {
		t.Fatalf("getJSON: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want two", got)
	}
	if out.Task.Status != "succeeded" {
		t.Fatalf("status = %q", out.Task.Status)
	}
}

// TestCreateRequestsAreNeverRetried is the cost guard: a transient failure on the
// video create must not be re-sent, because the server may already have accepted
// the task and started a paid render.
func TestCreateRequestsAreNeverRetried(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable"}}`))
	}))
	defer server.Close()

	settings := videoSettings(server.URL)
	settings.Retries = 3
	provider, err := New(settings)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.Generate(context.Background(),
		testRequest("MiniMax-H3", inference.OutputVideo, message.NewTextContent("a calm lake")))
	if err == nil {
		t.Fatal("503 on create returned no error")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want exactly one (a retried create bills twice)", got)
	}
}

// TestSpeechCreateIsNeverRetried covers the other paid create: synthesis is
// character-billed, so re-sending it is not free either.
func TestSpeechCreateIsNeverRetried(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	settings := speechSettings(server.URL)
	settings.Retries = 3
	provider, err := New(settings)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := provider.Generate(context.Background(),
		speechRequest("hello", &TTSOptions{Voice: "v"})); err == nil {
		t.Fatal("503 on speech create returned no error")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want exactly one", got)
	}
}

func TestRequestDoesNotRetryDeterministicFailures(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"base_resp":{"status_code":1004,"status_msg":"bad key"}}`))
	}))
	defer server.Close()

	settings := speechSettings(server.URL)
	settings.Retries = 3
	provider, err := New(settings)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.Generate(context.Background(), speechRequest("hello", &TTSOptions{Voice: "v"}))
	if err == nil {
		t.Fatal("unauthorized call returned no error")
	}
	if !errdefs.IsUnauthorized(err) {
		t.Fatalf("error = %v, want an unauthorized classification", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want one", got)
	}
}

func TestRequestKeepsCallerHeaders(t *testing.T) {
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Gateway")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":      map[string]any{"audio": "00"},
			"base_resp": map[string]any{"status_code": 0},
		})
	}))
	defer server.Close()

	settings := speechSettings(server.URL)
	settings.Headers = map[string]string{"X-Gateway": "edge-1"}
	provider, err := New(settings)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := provider.Generate(context.Background(),
		speechRequest("hello", &TTSOptions{Voice: "v"})); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if seen != "edge-1" {
		t.Fatalf("header = %q", seen)
	}
}

// TestJSONResponseBodyIsBounded pins the decode ceiling: an oversized body is
// reported as oversized, not as a truncated payload.
func TestJSONResponseBodyIsBounded(t *testing.T) {
	var out struct {
		Value int `json:"value"`
	}
	if err := decodeJSONWithin("probe", strings.NewReader(`{"value":1}`), 4, &out); !isErrorKind(err, inference.InvalidProviderResponse) {
		t.Fatalf("error = %v, want an invalid provider response", err)
	}
	out.Value = 0
	if err := decodeJSONWithin("probe", strings.NewReader(`{"value":1}`), 64, &out); err != nil {
		t.Fatalf("within-limit body rejected: %v", err)
	}
	if out.Value != 1 {
		t.Fatalf("value = %d", out.Value)
	}
	// Malformed JSON is a decode failure, not an oversize failure.
	if err := decodeJSONWithin("probe", strings.NewReader(`{`), 64, &out); !isErrorKind(err, inference.InvalidProviderResponse) {
		t.Fatalf("error = %v, want an invalid provider response", err)
	}
}

// TestStatusMessageIsTruncated pins the bound on provider text that rides an
// error: it lands in logs and in caller-visible errors.
func TestStatusMessageIsTruncated(t *testing.T) {
	err := baseResp{Code: statusBadParams, Message: strings.Repeat("x", 4000)}.
		err("speech synthesis")
	if err == nil {
		t.Fatal("non-zero status returned no error")
	}
	text := err.Error()
	if !strings.Contains(text, "(truncated)") {
		t.Fatalf("message was not truncated: %q", text)
	}
	if len([]rune(text)) > statusMessageLimit+128 {
		t.Fatalf("message is %d runes long", len([]rune(text)))
	}
	// An empty message keeps a stable placeholder rather than a bare colon.
	if empty := (baseResp{Code: statusBadParams}).err("op"); !strings.Contains(empty.Error(), "no message") {
		t.Fatalf("empty message = %q", empty)
	}
}

// TestRetryBudgetStopsRetrying pins the total-wait ceiling: a configured retry
// count is an attempt budget, not a licence to stall.
func TestRetryBudgetStopsRetrying(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := newMediaClient(Settings{BaseURL: server.URL, APIKey: "key", Retries: 10})
	client.budget = time.Millisecond
	var out struct{}
	err := client.getJSON(context.Background(), "/v2/query/video_generation/t1", &out)
	if err == nil {
		t.Fatal("unavailable poll returned no error")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want one: the budget is below the first backoff", got)
	}
	// The failure handed back is the provider's, not an interruption.
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("error = %v, want the provider's unavailable class", err)
	}
}
