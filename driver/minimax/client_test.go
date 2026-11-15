package minimax

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/driver/internal/httpx"
	"github.com/goccy/go-json"
)

// errTest is the cause the classification helpers wrap, so each case exercises
// the category marker rather than the message text.
var errTest = errors.New("boom")

// TestReadFailureIsReportedForTheRouterToRetry pins the split: the transport
// makes one attempt and hands the failure back classified, and `route` is what
// decides whether another target or another attempt is worth it.
func TestReadFailureIsReportedForTheRouterToRetry(t *testing.T) {
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

	client := newMediaClient(Settings{BaseURL: server.URL, APIKey: "key"})
	var out struct {
		Task struct {
			Status string `json:"status"`
		} `json:"task"`
	}
	err := client.getJSON(context.Background(), "/v2/query/video_generation/t1", &out)
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("error = %v, want a classified NotAvailable for the router", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want one: retrying belongs to route", got)
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

// TestUnaryLimitCoversASpeechAnswer pins the ceiling this driver asks the shared
// transport for: a unary speech answer carries the whole utterance as hex, so the
// limit is generous — but finite, because a broken gateway must not decide how
// much the driver allocates.
func TestUnaryLimitCoversASpeechAnswer(t *testing.T) {
	if unaryResponseLimit < 32<<20 {
		t.Fatalf("unary limit = %d, want at least the 32 MiB a long answer needs", unaryResponseLimit)
	}
	var out struct {
		Value int `json:"value"`
	}
	// Decoding itself is the transport's test; this only pins that the driver
	// asks for the bounded path at all.
	if err := httpx.DecodeJSON("probe", strings.NewReader(`{"value":1}`), unaryResponseLimit, &out); err != nil {
		t.Fatalf("within-limit body rejected: %v", err)
	}
	if out.Value != 1 {
		t.Fatalf("value = %d", out.Value)
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
