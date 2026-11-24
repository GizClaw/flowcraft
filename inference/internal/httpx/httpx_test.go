package httpx_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/inference/internal/httpx"
)

// A frame is dispatched on the blank line; multi-line data is joined with
// newlines, a comment is not a field, and a frame the stream ends inside is
// still delivered — servers do omit the trailing blank line, and dropping the
// last event is the kind of bug that only shows up on a short answer.
func TestFramesParsesTheFormat(t *testing.T) {
	body := strings.Join([]string{
		": keep-alive",
		"event: delta",
		"data: {\"a\":1}",
		"data: {\"b\":2}",
		"",
		"data:[DONE]",
	}, "\n")

	var got []httpx.Frame
	for frame, err := range httpx.Frames(strings.NewReader(body)) {
		if err != nil {
			t.Fatalf("frames: %v", err)
		}
		got = append(got, frame)
	}
	if len(got) != 2 {
		t.Fatalf("frames = %d, want 2: %+v", len(got), got)
	}
	if got[0].Event != "delta" {
		t.Errorf("event = %q, want delta", got[0].Event)
	}
	if want := "{\"a\":1}\n{\"b\":2}"; string(got[0].Data) != want {
		t.Errorf("data = %q, want %q", got[0].Data, want)
	}
	// The sentinel is the caller's convention, so it arrives unread.
	if string(got[1].Data) != "[DONE]" {
		t.Errorf("last frame = %q, want the unterminated [DONE]", got[1].Data)
	}
}

func TestFramesRejectsAnOversizedFrame(t *testing.T) {
	body := "data: " + strings.Repeat("x", (1<<20)+1) + "\n\n"
	for _, err := range httpx.Frames(strings.NewReader(body)) {
		if err == nil {
			t.Fatal("oversized frame accepted")
		}
		return
	}
	t.Fatal("oversized frame produced no error")
}

// TestFramesAcceptsALongLine covers the read-buffer boundary: a single line can
// run past it, which the scanner accumulates rather than dropping, and the frame
// after it must still parse from a clean buffer.
func TestFramesAcceptsALongLine(t *testing.T) {
	long := strings.Repeat("x", 40<<10)
	body := "data: " + long + "\n\ndata: short\n\n"
	var frames []httpx.Frame
	for frame, err := range httpx.Frames(strings.NewReader(body)) {
		if err != nil {
			t.Fatalf("Frames: %v", err)
		}
		frames = append(frames, frame)
	}
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	if got := string(frames[0].Data); got != long {
		t.Fatalf("first frame = %d bytes, want %d", len(got), len(long))
	}
	if got := string(frames[1].Data); got != "short" {
		t.Fatalf("second frame = %q, want short", got)
	}
}

// Do classifies what the layer above has to decide about: the status when the
// body says nothing, the vendor's envelope when it does.
func TestDoClassifiesFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer key" {
			t.Errorf("authorization = %q, want the bearer token", got)
		}
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":42}`))
		case "/unauthorized":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		case "/limited":
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		case "/enveloped":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":2013}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := httpx.New(httpx.Config{
		BaseURL: server.URL,
		APIKey:  "key",
		DecodeError: func(_ int, body []byte) error {
			if strings.Contains(string(body), "2013") {
				return errdefs.Validationf("bad parameters")
			}
			return nil
		},
	})
	ctx := context.Background()

	var decoded struct {
		Value int `json:"value"`
	}
	if err := client.JSON(ctx, httpx.Op{Method: http.MethodGet, Path: "/ok"}, &decoded); err != nil {
		t.Fatalf("ok: %v", err)
	}
	if decoded.Value != 42 {
		t.Errorf("value = %d, want 42", decoded.Value)
	}

	_, err := client.Do(ctx, httpx.Op{Method: http.MethodGet, Path: "/unauthorized"})
	if !errdefs.IsUnauthorized(err) {
		t.Errorf("401 = %v, want Unauthorized", err)
	}

	_, err = client.Do(ctx, httpx.Op{Method: http.MethodGet, Path: "/limited"})
	if !errdefs.IsRateLimit(err) {
		t.Fatalf("429 = %v, want RateLimit", err)
	}
	if hint, ok := errdefs.RetryAfter(err); !ok || hint != 7*time.Second {
		t.Errorf("Retry-After = %v (%v), want 7s", hint, ok)
	}

	// The vendor's envelope outranks the status: this body names the reason the
	// request was refused, and that reason is what a retry layer must not retry.
	_, err = client.Do(ctx, httpx.Op{Method: http.MethodGet, Path: "/enveloped"})
	if !errdefs.IsValidation(err) {
		t.Errorf("enveloped 400 = %v, want the decoded Validation", err)
	}
}

// An oversized body is refused rather than decoded from a prefix: a truncated
// payload that happens to parse would be a wrong answer, not a short one.
func TestJSONRefusesAnOversizedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"value":"` + strings.Repeat("x", 4096) + `"}`))
	}))
	defer server.Close()

	client := httpx.New(httpx.Config{BaseURL: server.URL, Limit: 128})
	var out map[string]any
	if err := client.JSON(context.Background(), httpx.Op{Method: http.MethodGet, Path: "/"}, &out); err == nil {
		t.Fatal("oversized body accepted")
	}
}

// A cancellation is the caller's, not the far end's: reporting it as a transport
// failure would let a retry layer repeat abandoned work.
func TestDoReportsACancelledCallAsInterrupted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := httpx.New(httpx.Config{BaseURL: server.URL}).Do(ctx,
		httpx.Op{Method: http.MethodGet, Path: "/slow"})
	if !errdefs.IsInterrupted(err) {
		t.Errorf("cancelled call = %v, want Interrupted", err)
	}
}

// The poller stops at the first terminal answer and hands back its value; a task
// that ends by failing returns the failure instead of the value.
func TestPollDrivesATaskToItsTerminalState(t *testing.T) {
	asked := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked++
		state := "running"
		if asked >= 3 {
			state = "succeeded"
		}
		_, _ = w.Write([]byte(`{"state":"` + state + `"}`))
	}))
	defer server.Close()

	type task struct {
		State string `json:"state"`
	}
	client := httpx.New(httpx.Config{BaseURL: server.URL})
	done, err := httpx.Poll(context.Background(), client, httpx.Await[task]{
		Op:       httpx.Op{Method: http.MethodGet, Path: "/task"},
		Interval: time.Millisecond,
		Budget:   5 * time.Second,
		Done: func(value task) (bool, error) {
			switch value.State {
			case "succeeded":
				return true, nil
			case "failed":
				return true, errors.New("task failed")
			default:
				return false, nil
			}
		},
	})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if done.State != "succeeded" || asked != 3 {
		t.Errorf("state = %q after %d queries, want succeeded after 3", done.State, asked)
	}
}

// Stream is the call a driver makes before reading frames: it validates the
// status like any other call and hands back the body for [Frames].
func TestStreamReturnsTheEventBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: delta\ndata: 1\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	body, err := httpx.New(httpx.Config{BaseURL: server.URL}).Stream(context.Background(),
		httpx.Op{Method: http.MethodPost, Path: "/stream"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = body.Close() }()
	count := 0
	for frame, err := range httpx.Frames(body) {
		if err != nil {
			t.Fatalf("frames: %v", err)
		}
		count++
		if frame.Event == "delta" && string(frame.Data) != "1" {
			t.Errorf("frame = %+v", frame)
		}
	}
	if count != 2 {
		t.Fatalf("frames = %d, want 2", count)
	}
}

// Bytes is the artifact read, and the ceiling it takes is the caller's: an
// artifact is not a provider response, so its size is the surface's business.
func TestBytesReturnsTheBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/file":
			_, _ = w.Write([]byte("image-bytes"))
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
		case "/expired":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("link expired"))
		}
	}))
	defer server.Close()
	client := httpx.New(httpx.Config{BaseURL: server.URL})
	ctx := context.Background()

	body, err := client.Bytes(ctx, httpx.Op{Method: http.MethodGet, Path: "/file"}, 128)
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if string(body) != "image-bytes" {
		t.Errorf("body = %q", body)
	}

	// A call that names no ceiling falls back to the client's: the small body is
	// accepted under it, the big one is not. Both halves are needed — an
	// implementation that treated 0 as "refuse everything" would pass the refusal
	// alone.
	bounded := httpx.New(httpx.Config{BaseURL: server.URL, Limit: 128})
	if _, err := bounded.Bytes(ctx, httpx.Op{Method: http.MethodGet, Path: "/file"}, 0); err != nil {
		t.Errorf("a body under the client's ceiling was refused: %v", err)
	}
	_, err = bounded.Bytes(ctx, httpx.Op{Method: http.MethodGet, Path: "/big"}, 0)
	if err == nil {
		t.Fatal("an oversized artifact was accepted under the client's ceiling")
	}
	// Refused, not truncated: the category says the provider answered with
	// something this driver will not read.
	if !inference.IsKind(err, inference.InvalidProviderResponse) {
		t.Errorf("oversized artifact = %v, want InvalidProviderResponse", err)
	}
	// And the caller's ceiling wins over the client's, in both directions.
	if _, err := client.Bytes(ctx, httpx.Op{Method: http.MethodGet, Path: "/big"}, 64); err == nil {
		t.Error("an artifact past the caller's ceiling was accepted")
	}
	if _, err := bounded.Bytes(ctx, httpx.Op{Method: http.MethodGet, Path: "/big"}, 8192); err != nil {
		t.Errorf("a body under the caller's larger ceiling was refused: %v", err)
	}
	// Exactly at the ceiling is inside it: the limit is a maximum, not an
	// exclusive bound, and an off-by-one here would truncate a legitimate file.
	if body, err := client.Bytes(ctx, httpx.Op{Method: http.MethodGet, Path: "/file"}, 11); err != nil || len(body) != 11 {
		t.Errorf("a body of exactly the ceiling = %d bytes (%v), want 11 accepted", len(body), err)
	}
	if _, err := client.Bytes(ctx, httpx.Op{Method: http.MethodGet, Path: "/file"}, 10); err == nil {
		t.Error("a body one byte past the ceiling was accepted")
	}

	// A dead link is a classification, not a body.
	_, err = client.Bytes(ctx, httpx.Op{Method: http.MethodGet, Path: "/expired"}, 128)
	if !errdefs.IsForbidden(err) {
		t.Errorf("expired link = %v, want a classified refusal", err)
	}
}

// Stream validates like every other call: a non-2xx is an error, and the caller
// never sees a body it would parse as frames.
func TestStreamClassifiesANonSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	}))
	defer server.Close()

	body, err := httpx.New(httpx.Config{BaseURL: server.URL}).Stream(context.Background(),
		httpx.Op{Method: http.MethodPost, Path: "/stream"})
	if err == nil {
		_ = body.Close()
		t.Fatal("a 429 was handed back as a stream")
	}
	// A caller reads the body without checking err first, so a refusal must not
	// hand one back: nil is the only safe answer alongside an error.
	if body != nil {
		_ = body.Close()
		t.Errorf("stream refusal returned a body alongside its error: %v", err)
	}
	if !errdefs.IsRateLimit(err) {
		t.Errorf("stream 429 = %v, want RateLimit", err)
	}
	if hint, ok := errdefs.RetryAfter(err); !ok || hint != 3*time.Second {
		t.Errorf("Retry-After = %v (%v), want 3s", hint, ok)
	}
}

// A task that never finishes is bounded by the budget, and the bound is the
// poller's own: once it returns, no further query goes out.
func TestPollStopsAtTheBudget(t *testing.T) {
	var asked atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		_, _ = w.Write([]byte(`{"state":"running"}`))
	}))
	defer server.Close()

	type task struct {
		State string `json:"state"`
	}
	_, err := httpx.Poll(context.Background(), httpx.New(httpx.Config{BaseURL: server.URL}),
		httpx.Await[task]{
			Op:       httpx.Op{Method: http.MethodGet, Path: "/task"},
			Interval: time.Millisecond,
			Budget:   30 * time.Millisecond,
			Done:     func(task) (bool, error) { return false, nil },
		})
	if err == nil {
		t.Fatal("an unfinished task was reported as done")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Errorf("budget expiry = %v, want NotAvailable", err)
	}
	if !strings.Contains(err.Error(), "30ms") {
		t.Errorf("error = %v, want it to name the budget it was given", err)
	}
	// The loop has stopped, not merely returned: a goroutine still polling would
	// keep spending rate limit after the caller gave up.
	settled := asked.Load()
	time.Sleep(50 * time.Millisecond)
	if after := asked.Load(); after != settled {
		t.Errorf("queries went from %d to %d after the poller returned", settled, after)
	}
}

// The caller's context is the other bound, and it is honoured where the wait
// happens: a cancel during the interval returns at once, as the caller's abort
// rather than as anything the far end said.
//
// The interval is long enough that a poller ignoring the context would have to
// wait it out, which is what the elapsed bound measures.
func TestPollStopsWhenTheCallerCancels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":"running"}`))
	}))
	defer server.Close()

	type task struct {
		State string `json:"state"`
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	started := time.Now()
	_, err := httpx.Poll(ctx, httpx.New(httpx.Config{BaseURL: server.URL}), httpx.Await[task]{
		Op:       httpx.Op{Method: http.MethodGet, Path: "/task"},
		Interval: 500 * time.Millisecond,
		Done:     func(task) (bool, error) { return false, nil },
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled poll = %v, want the caller's cancellation", err)
	}
	if !errdefs.IsAborted(err) {
		t.Errorf("cancelled poll = %v, want Aborted", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Errorf("the poll returned after %s, want it to notice the cancel during the 500ms interval", elapsed)
	}
}

// A task that ends by failing returns its failure rather than its value, and it
// is not the budget that stopped the wait.
func TestPollReturnsATaskFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":"failed","reason":"the render was rejected"}`))
	}))
	defer server.Close()

	type task struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
	}
	_, err := httpx.Poll(context.Background(), httpx.New(httpx.Config{BaseURL: server.URL}),
		httpx.Await[task]{
			Op:       httpx.Op{Method: http.MethodGet, Path: "/task"},
			Interval: time.Millisecond,
			Budget:   5 * time.Second,
			Done: func(value task) (bool, error) {
				if value.State == "failed" {
					return true, fmt.Errorf("task failed: %s", value.Reason)
				}
				return false, nil
			},
		})
	if err == nil {
		t.Fatal("a failed task was reported as done")
	}
	if !strings.Contains(err.Error(), "the render was rejected") {
		t.Errorf("error = %v, want the platform's reason", err)
	}
}

// A poller without a completion check would loop until the budget ran out and
// then call the task unfinished, which says nothing about the caller's mistake.
func TestPollRequiresACompletionCheck(t *testing.T) {
	_, err := httpx.Poll(context.Background(), httpx.New(httpx.Config{}),
		httpx.Await[map[string]any]{Op: httpx.Op{Method: http.MethodGet, Path: "/task"}})
	if !errdefs.IsValidation(err) {
		t.Errorf("missing Done = %v, want Validation", err)
	}
}
