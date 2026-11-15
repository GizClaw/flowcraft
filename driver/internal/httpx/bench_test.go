package httpx_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/driver/internal/httpx"
)

// benchStream builds n frames shaped like a chat delta: the payload every
// streaming driver parses, at the size vendors actually send.
func benchStream(frames int) []byte {
	payload := `data: {"choices":[{"index":0,"delta":{"content":"the quick brown fox "}}]}` + "\n\n"
	var body strings.Builder
	body.Grow(frames * len(payload))
	for range frames {
		body.WriteString(payload)
	}
	return []byte(body.String())
}

// BenchmarkFrames is the stream scan: one pass over a body a real answer
// produces. Reports bytes so throughput is comparable against the wall clock
// vendors take to send it.
func BenchmarkFrames(b *testing.B) {
	body := benchStream(200)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		frames := 0
		for frame, err := range httpx.Frames(bytes.NewReader(body)) {
			if err != nil {
				b.Fatal(err)
			}
			if len(frame.Data) == 0 {
				b.Fatal("empty frame")
			}
			frames++
		}
		if frames != 200 {
			b.Fatalf("frames = %d, want 200", frames)
		}
	}
}

type benchImage struct {
	Data    []benchImageItem `json:"data"`
	Created int64            `json:"created"`
	Usage   struct {
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

type benchImageItem struct {
	URL     string `json:"url"`
	B64JSON string `json:"b64_json"`
}

// BenchmarkDecodeJSON is the response decode every non-streaming call runs,
// including the limit check that keeps a vendor from deciding how much is read.
func BenchmarkDecodeJSON(b *testing.B) {
	payload := `{"created":1731000000,"usage":{"output_tokens":1440},"data":[`
	items := make([]string, 0, 4)
	for range 4 {
		items = append(items, `{"url":"https://ark-cn-beijing.bytedance.com/api/v3/files/`+
			strings.Repeat("a", 96)+`.jpeg"}`)
	}
	body := payload + strings.Join(items, ",") + `]}`
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		var response benchImage
		if err := httpx.DecodeJSON("images", strings.NewReader(body), 1<<20, &response); err != nil {
			b.Fatal(err)
		}
		if len(response.Data) != 4 {
			b.Fatalf("items = %d, want 4", len(response.Data))
		}
	}
}

// BenchmarkDoJSON is the whole request path against a loopback server: header
// assembly, the round trip, and the decode. It is the number a driver's latency
// pays for the shared layer, so a regression here is a regression everywhere.
func BenchmarkDoJSON(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"task-1","state":"running"}`))
	}))
	defer server.Close()
	client := httpx.New(httpx.Config{
		BaseURL: server.URL,
		APIKey:  "bench",
		Headers: map[string]string{"X-Project-Name": "bench"},
	})
	type task struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	body := map[string]any{"model": "m", "content": []any{map[string]any{"type": "text", "text": "hi"}}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var answer task
		if err := client.JSON(context.Background(), httpx.Op{
			Method: http.MethodPost, Path: "/tasks", Body: body,
		}, &answer); err != nil {
			b.Fatal(err)
		}
	}
}
