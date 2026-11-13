package minimax

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// videoSettings builds a deployment that serves one H3 model against base.
func videoSettings(base string) Settings {
	return Settings{
		BaseURL: base,
		APIKey:  "test-key",
		Models:  []ModelSpec{h3Spec()},
	}
}

func TestVideoCreatesPollsAndReturnsTheArtifact(t *testing.T) {
	var (
		created map[string]any
		queries int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == videoPathCreate:
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Errorf("decode create: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "task-1"})
		case r.Method == http.MethodGet && r.URL.Path == videoPathQueryPrefix+"task-1":
			queries++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"task": map[string]any{
					"id":      "task-1",
					"status":  "succeeded",
					"content": map[string]any{"url": "https://cdn.example.com/clip.mp4"},
					"usage": map[string]any{
						"prompt_tokens": 5, "completion_tokens": 7, "total_tokens": 12,
					},
				},
			})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider, err := New(videoSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := testRequest("MiniMax-H3", inference.OutputVideo, message.NewTextContent("a calm lake"))
	request.Extensions = inference.Extensions{&VideoOptions{
		DurationMillis: int64Ptr(10000),
		Resolution:     "2K",
		AspectRatio:    "16:9",
		Watermark:      boolPtr(true),
		CallbackURL:    "https://example.com/cb",
	}}
	response, err := provider.Generate(context.Background(), request)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if queries != 1 {
		t.Fatalf("queries = %d, want one", queries)
	}
	if created["model"] != "MiniMax-H3" {
		t.Fatalf("model = %v", created["model"])
	}
	if created["duration"] != float64(10) ||
		created["resolution"] != "2K" || created["ratio"] != "16:9" {
		t.Fatalf("create = %+v", created)
	}
	if created["aigc_watermark"] != true || created["callback_url"] != "https://example.com/cb" {
		t.Fatalf("create = %+v", created)
	}
	content, ok := created["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %+v", created["content"])
	}
	text := content[0].(map[string]any)
	if text["type"] != "text" || text["text"] != "a calm lake" {
		t.Fatalf("content[0] = %+v", text)
	}

	if response.Metadata.RequestID != "task-1" {
		t.Fatalf("request id = %q", response.Metadata.RequestID)
	}
	if response.Usage.OutputTokens != 7 {
		t.Fatalf("output tokens = %d", response.Usage.OutputTokens)
	}
	if got := response.Usage.Work[usage.WorkVideosGenerated]; got != 1 {
		t.Fatalf("videos generated = %d", got)
	}
	part, ok := response.Message.Content.Parts[0].(message.VideoPart)
	if !ok {
		t.Fatalf("part = %T", response.Message.Content.Parts[0])
	}
	if part.Source.URL() != "https://cdn.example.com/clip.mp4" {
		t.Fatalf("video url = %q", part.Source.URL())
	}
	if part.Source.MediaType() != "video/mp4" {
		t.Fatalf("media type = %q", part.Source.MediaType())
	}
}

func TestVideoSendsInlineMediaAsDataURIs(t *testing.T) {
	var created map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case videoPathCreate:
			_ = json.NewDecoder(r.Body).Decode(&created)
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "task-2"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"task": map[string]any{
					"id": "task-2", "status": "succeeded",
					"content": map[string]any{"url": "https://cdn.example.com/clip.mp4"},
				},
			})
		}
	}))
	defer server.Close()

	provider, err := New(videoSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The text part comes first so the prompt is not empty.
	request := testRequest("MiniMax-H3", inference.OutputVideo, message.Content{
		Parts: []message.Part{
			message.TextPart{Text: "animate this"},
			message.ImagePart{Source: mustImageBytes(t, []byte("png-bytes"))},
		},
	})
	if _, err := provider.Generate(context.Background(), request); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	content := created["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %+v", content)
	}
	image := content[1].(map[string]any)
	if image["type"] != "image_url" || image["role"] != "first_frame" {
		t.Fatalf("image item = %+v", image)
	}
	nested := image["image_url"].(map[string]any)
	url := nested["url"].(string)
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	if url != want {
		t.Fatalf("url = %q, want a data URI", url)
	}
}

func TestVideoReportsServerFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == videoPathCreate {
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "task-3"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"task": map[string]any{"id": "task-3", "status": "failed"},
		})
	}))
	defer server.Close()

	provider, err := New(videoSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.Generate(context.Background(),
		testRequest("MiniMax-H3", inference.OutputVideo, message.NewTextContent("a calm lake")))
	if err == nil {
		t.Fatal("failed task returned no error")
	}
}

func TestVideoReportsMissingArtifact(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == videoPathCreate {
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "task-4"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"task": map[string]any{"id": "task-4", "status": "succeeded", "content": map[string]any{}},
		})
	}))
	defer server.Close()

	provider, err := New(videoSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.Generate(context.Background(),
		testRequest("MiniMax-H3", inference.OutputVideo, message.NewTextContent("a calm lake")))
	if !isErrorKind(err, inference.InvalidProviderResponse) {
		t.Fatalf("error = %v, want an invalid provider response", err)
	}
}

func TestVideoCreateHTTPFailureIsClassified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"base_resp":{"status_code":1002,"status_msg":"rate limited"}}`)
	}))
	defer server.Close()

	provider, err := New(videoSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.Generate(context.Background(),
		testRequest("MiniMax-H3", inference.OutputVideo, message.NewTextContent("a calm lake")))
	if err == nil {
		t.Fatal("HTTP 429 returned no error")
	}
}

func TestVideoWaitsForATerminalStatus(t *testing.T) {
	queries := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == videoPathCreate {
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "task-5"})
			return
		}
		queries++
		status := "processing"
		content := map[string]any{}
		if queries >= 2 {
			status = "succeeded"
			content = map[string]any{"url": "https://cdn.example.com/clip.mp4"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"task": map[string]any{"id": "task-5", "status": status, "content": content},
		})
	}))
	defer server.Close()

	settings := videoSettings(server.URL)
	settings.VideoPollInterval = time.Millisecond
	provider, err := New(settings)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := provider.Generate(context.Background(),
		testRequest("MiniMax-H3", inference.OutputVideo, message.NewTextContent("a calm lake"))); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if queries != 2 {
		t.Fatalf("queries = %d, want two", queries)
	}
}

func mustImageBytes(t *testing.T, data []byte) media.ImageSource {
	t.Helper()
	source, err := media.NewImageBytes(data, "image/png")
	if err != nil {
		t.Fatalf("inline image: %v", err)
	}
	return source
}

// TestVideoCreateEnvelopeFailureIsClassified covers the v2 create answering HTTP
// 200 with a failure envelope: it must surface the platform's class, not a
// driver-bug "no task_id" error.
func TestVideoCreateEnvelopeFailureIsClassified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"task_id":   "",
			"base_resp": map[string]any{"status_code": statusNoBalance, "status_msg": "no balance"},
		})
	}))
	defer server.Close()

	provider, err := New(videoSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.Generate(context.Background(),
		testRequest("MiniMax-H3", inference.OutputVideo, message.NewTextContent("a calm lake")))
	if err == nil {
		t.Fatal("create with a failure envelope returned no error")
	}
	if !errdefs.IsForbidden(err) {
		t.Fatalf("error = %v, want the platform's forbidden class", err)
	}
	if isErrorKind(err, inference.InvalidProviderResponse) {
		t.Fatal("a refusal was reported as an invalid provider response")
	}
}

// TestVideoQueryEnvelopeFailureIsClassified covers the same envelope on the query
// endpoint, which otherwise polls until the caller's deadline.
func TestVideoQueryEnvelopeFailureIsClassified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == videoPathCreate {
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "task-env"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"base_resp": map[string]any{"status_code": statusRateLimited, "status_msg": "slow down"},
		})
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	provider, err := New(videoSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.Generate(ctx,
		testRequest("MiniMax-H3", inference.OutputVideo, message.NewTextContent("a calm lake")))
	if !errdefs.IsRateLimit(err) {
		t.Fatalf("error = %v, want the platform's rate-limit class", err)
	}
}

// TestVideoQueryWithoutStatusFails covers a statusless task: waiting on it would
// spin to the deadline and report an interruption instead of the real answer.
func TestVideoQueryWithoutStatusFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == videoPathCreate {
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "task-nostatus"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"task": map[string]any{"id": "task-nostatus"},
		})
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	provider, err := New(videoSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.Generate(ctx,
		testRequest("MiniMax-H3", inference.OutputVideo, message.NewTextContent("a calm lake")))
	if !isErrorKind(err, inference.InvalidProviderResponse) {
		t.Fatalf("error = %v, want an invalid provider response (and no polling)", err)
	}
}
