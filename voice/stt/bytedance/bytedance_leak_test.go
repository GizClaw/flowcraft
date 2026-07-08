package bytedance

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/voice/audio"
	"github.com/coder/websocket"
	"go.uber.org/goleak"
)

// wsTestURL converts an httptest server URL to a ws:// URL.
func wsTestURL(httpURL string) string {
	return strings.Replace(httpURL, "http://", "ws://", 1)
}

// singleFrameInput returns a closed input pipe carrying one audio frame, which
// drives RecognizeStream's writer to send the frame then a finish marker.
func singleFrameInput() *audio.Pipe[audio.Frame] {
	input := audio.NewPipe[audio.Frame](2)
	input.Send(audio.Frame{
		Data:   make([]byte, 3200),
		Format: audio.Format{SampleRate: 16000, Channels: 1, BitDepth: 16},
	})
	input.Close()
	return input
}

// loopingFrameStream yields empty-data frames forever — it never returns io.EOF
// and never blocks. This keeps RecognizeStream's writer goroutine spinning in
// its read loop with NO side effects: because Data is empty the writer never
// calls sendAudio, so the writer's only clean exit is the `innerCtx.Err()`
// guard before input.Read (the fix under test), which the reader trips via its
// deferred innerCancel on early/server-error/ctx-cancel exit. Frames carrying
// real data would let the writer also exit via a sendAudio error after the
// connection closes, masking a missing guard — empty frames isolate the guard.
//
// It owns no goroutine, so there is nothing for the test itself to leak. The
// tiny pause avoids a hot CPU spin without affecting the guarantees. The first
// frame's Format is what RecognizeStream reads to build the ASR request.
type loopingFrameStream struct {
	format audio.Format
}

func (s loopingFrameStream) Read() (audio.Frame, error) {
	time.Sleep(time.Millisecond)
	return audio.Frame{Format: s.format}, nil
}

func loopingInput() loopingFrameStream {
	return loopingFrameStream{format: audio.Format{SampleRate: 16000, Channels: 1, BitDepth: 16}}
}

// TestRecognizeStream_NoLeak_NormalCompletion locks in the writer/reader
// goroutine teardown on normal completion (finish sent, final frame received,
// output drained to EOF).
func TestRecognizeStream_NoLeak_NormalCompletion(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(websocket.StatusNormalClosure, "") }()

		if _, _, err := c.Read(r.Context()); err != nil {
			return
		}
		// Consume audio until the finish (negative-seq) frame.
		for {
			_, raw, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if len(raw) < 8 {
				continue
			}
			if binary.BigEndian.Uint32(raw[0:4])&msgTypeFlagMask == flagNegativeSeq {
				break
			}
		}
		final := asrResponsePayload{}
		final.Result.Utterances = []asrUtterance{{Text: "你好", Definite: true}}
		data, _ := json.Marshal(final)
		_ = c.Write(r.Context(), websocket.MessageBinary, buildServerFullFrame(data, true))
	}))
	defer srv.Close()

	s, _ := New(WithAppID("app"), WithToken("tok"), WithHost(wsTestURL(srv.URL)))
	out, err := s.RecognizeStream(context.Background(), singleFrameInput())
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := out.Read(); err != nil {
			break
		}
	}
}

// TestRecognizeStream_NoLeak_ServerError locks in the writer-goroutine teardown
// on early server-error termination (the fix under test). The input stream
// keeps yielding frames forever (loopingInput), so the writer never reaches its
// own io.EOF/send-finish exit; teardown of the writer depends entirely on the
// reader's deferred innerCancel unblocking the `innerCtx.Err()` guard. If that
// guard is removed the writer spins forever and goleak fires.
func TestRecognizeStream_NoLeak_ServerError(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(websocket.StatusNormalClosure, "") }()

		if _, _, err := c.Read(r.Context()); err != nil {
			return
		}
		errPayload, _ := json.Marshal(asrResponsePayload{Error: "quota exceeded"})
		_ = c.Write(r.Context(), websocket.MessageBinary, buildErrorFrame(1013, errPayload))
		// Keep the connection open so the client observes the error frame
		// before the normal-closure handshake.
		_, _, _ = c.Read(r.Context())
	}))
	defer srv.Close()

	s, _ := New(WithAppID("app"), WithToken("tok"), WithHost(wsTestURL(srv.URL)))
	out, err := s.RecognizeStream(context.Background(), loopingInput())
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := out.Read(); err != nil {
			break
		}
	}
}

// TestRecognizeStream_NoLeak_CtxCancelled locks in goroutine teardown when the
// caller cancels the context while the reader is blocked awaiting results. The
// input stream keeps yielding frames forever (loopingInput), so the writer
// stays looping and can only exit via the `innerCtx.Err()` guard once the
// reader's deferred innerCancel fires on ctx cancel. If that guard is removed
// the writer spins forever and goleak fires.
func TestRecognizeStream_NoLeak_CtxCancelled(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(websocket.StatusNormalClosure, "") }()
		// Consume everything (request, audio, finish) and then keep the
		// connection open, never sending a result, so the client reader blocks
		// until the caller cancels.
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	s, _ := New(WithAppID("app"), WithToken("tok"), WithHost(wsTestURL(srv.URL)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := s.RecognizeStream(ctx, loopingInput())
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	for {
		if _, err := out.Read(); err != nil {
			break
		}
	}
}
