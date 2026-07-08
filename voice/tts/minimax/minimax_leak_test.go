package minimax

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	speechaudio "github.com/GizClaw/flowcraft/voice/audio"
	"go.uber.org/goleak"
)

// newLeakTestTTS builds a TTS whose HTTP client uses a dedicated Transport so
// each test can deterministically close its idle connections before goleak
// verification (the default http.DefaultClient pools connections globally,
// whose reaper goroutines would otherwise be reported as false positives).
func newLeakTestTTS(t *testing.T, baseURL string) (*TTS, *http.Transport) {
	t.Helper()
	p, err := New(WithAPIKey("test-key"), WithBaseURL(baseURL))
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{}
	p.client = &http.Client{Transport: tr}
	return p, tr
}

func newSSEServer(handler func(w http.ResponseWriter, flush func())) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		flush := func() {
			if flusher != nil {
				flusher.Flush()
			}
		}
		handler(w, flush)
	}))
}

func writeSSE(w http.ResponseWriter, flush func(), ev t2aResponse) {
	data, _ := json.Marshal(ev)
	_, _ = w.Write([]byte("data: " + string(data) + "\n\n"))
	flush()
}

// TestSynthesizeStream_NoLeak_NormalCompletion locks in the fix that the
// ctx-watcher goroutine and synth loop exit on normal completion (input closed
// -> EOF -> output drained to EOF).
func TestSynthesizeStream_NoLeak_NormalCompletion(t *testing.T) {
	defer goleak.VerifyNone(t) // runs last (see defer ordering below)

	chunk1 := hex.EncodeToString([]byte("chunk-1"))
	chunk2 := hex.EncodeToString([]byte("chunk-2"))
	srv := newSSEServer(func(w http.ResponseWriter, flush func()) {
		writeSSE(w, flush, t2aResponse{Data: &t2aData{Audio: chunk1, Status: 1}, BaseResp: &baseResp{StatusCode: 0}})
		writeSSE(w, flush, t2aResponse{Data: &t2aData{Audio: chunk2, Status: 1}, BaseResp: &baseResp{StatusCode: 0}})
		writeSSE(w, flush, t2aResponse{Data: &t2aData{Audio: "", Status: 2}, BaseResp: &baseResp{StatusCode: 0, StatusMsg: "success"}})
	})
	defer srv.Close()

	p, tr := newLeakTestTTS(t, srv.URL)
	defer tr.CloseIdleConnections()

	textPipe := speechaudio.NewPipe[string](1)
	textPipe.Send("hello")
	textPipe.Close()

	stream, err := p.SynthesizeStream(context.Background(), textPipe)
	if err != nil {
		t.Fatal(err)
	}

	var chunks int
	for {
		_, err := stream.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		chunks++
	}
	if chunks != 2 {
		t.Fatalf("got %d chunks, want 2", chunks)
	}
}

// TestSynthesizeStream_NoLeak_CtxCancelledMidStream locks in the fix that a
// caller-ctx cancellation mid-stream tears down both goroutines instead of
// leaking them, and that teardown surfaces via the ctx-watcher (context.Canceled)
// rather than only via an eventual transport abort.
//
// The SSE handler sends exactly one chunk then BLOCKS (never sends more, never
// returns) until test cleanup, so the worker is genuinely parked mid-stream —
// blocked reading the next SSE line — when the caller ctx is cancelled. The
// load-bearing production fix here is the done-channel watcher goroutine in
// SynthesizeStream: on ctx.Done() it calls out.Interrupt(), which unblocks the
// consumer's Read with context.Canceled promptly. If that watcher is removed,
// the consumer never observes context.Canceled (it would instead see a delayed
// io.EOF from the transport-abort path, or block), so the assertion below fails.
func TestSynthesizeStream_NoLeak_CtxCancelledMidStream(t *testing.T) {
	defer goleak.VerifyNone(t)

	chunk := hex.EncodeToString([]byte("chunk-1"))
	// release unblocks the handler at cleanup so the server goroutine never
	// outlives the test (otherwise goleak would flag the parked handler).
	release := make(chan struct{})
	srv := newSSEServer(func(w http.ResponseWriter, flush func()) {
		// Deliver exactly one chunk, then park so the worker stays genuinely
		// mid-stream (blocked on the next SSE line) until the test releases it.
		writeSSE(w, flush, t2aResponse{Data: &t2aData{Audio: chunk, Status: 1}, BaseResp: &baseResp{StatusCode: 0}})
		<-release
	})
	// Ordering (LIFO): CloseIdleConnections -> close(release) -> srv.Close ->
	// goleak.VerifyNone. close(release) lets the parked handler return so
	// srv.Close() can join it before the leak check runs.
	defer srv.Close()
	defer close(release)

	p, tr := newLeakTestTTS(t, srv.URL)
	defer tr.CloseIdleConnections()

	ctx, cancel := context.WithCancel(context.Background())
	textPipe := speechaudio.NewPipe[string](1)
	textPipe.Send("hello")
	textPipe.Close()

	stream, err := p.SynthesizeStream(ctx, textPipe)
	if err != nil {
		t.Fatal(err)
	}

	// Read the first delivered chunk, confirming the worker is now mid-stream.
	if _, err := stream.Read(); err != nil {
		t.Fatalf("first read: %v", err)
	}

	// Cancel mid-stream. The ctx-watcher must interrupt the output promptly.
	cancel()

	errCh := make(chan error, 1)
	go func() {
		var last error
		for {
			if _, err := stream.Read(); err != nil {
				last = err
				break
			}
		}
		errCh <- last
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("terminal error = %v, want context.Canceled (ctx-watcher must interrupt mid-stream)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not terminate promptly after ctx cancel (worker leaked mid-stream)")
	}
}

// TestSynthesizeStream_NoLeak_ProviderError locks in the fix that a provider
// error mid-stream tears down the goroutines (here the server returns a
// base_resp error after some audio).
func TestSynthesizeStream_NoLeak_ProviderError(t *testing.T) {
	defer goleak.VerifyNone(t)

	chunk := hex.EncodeToString([]byte("chunk-1"))
	srv := newSSEServer(func(w http.ResponseWriter, flush func()) {
		writeSSE(w, flush, t2aResponse{Data: &t2aData{Audio: chunk, Status: 1}, BaseResp: &baseResp{StatusCode: 0}})
		// Provider error frame mid-stream.
		writeSSE(w, flush, t2aResponse{BaseResp: &baseResp{StatusCode: 1002, StatusMsg: "rate limited"}})
	})
	defer srv.Close()

	p, tr := newLeakTestTTS(t, srv.URL)
	defer tr.CloseIdleConnections()

	textPipe := speechaudio.NewPipe[string](1)
	textPipe.Send("hello")
	textPipe.Close()

	stream, err := p.SynthesizeStream(context.Background(), textPipe)
	if err != nil {
		t.Fatal(err)
	}

	// Drain to the terminal error. The provider-error path interrupts the
	// output, so the terminal Read reports context.Canceled (abnormal
	// termination); this test only asserts no goroutine leaks on that path.
	for {
		if _, err := stream.Read(); err != nil {
			break
		}
	}
}
