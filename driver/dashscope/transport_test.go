package dashscope

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// recordedCall is one wire call the fake platform saw.
type recordedCall struct {
	method string
	path   string
	auth   string
	async  string
	sse    string
	work   string
	body   map[string]any
}

// fakePlatform is a scripted DashScope host: it answers task creates, task
// queries, and the sync multimodal call, recording every request so the tests
// can assert the headers and payloads the driver sends.
type fakePlatform struct {
	mu    sync.Mutex
	calls []recordedCall
	// base is the server's own address, filled in when the fake starts; a
	// responder that has to name a link the driver will fetch — the
	// transcription result file — builds it from here.
	base string

	create func(body map[string]any) (int, string)
	query  func(taskID string) (int, string)
	sync   func(body map[string]any) (int, string)
	// generate answers the text surfaces' unary calls, stream the
	// event-stream shape they take when the sse header is set, and embed
	// the text-embedding surface.
	generate func(body map[string]any) (int, string)
	embed    func(body map[string]any) (int, string)
	stream   func(body map[string]any) (int, string)
	// artifact answers the result-file fetches: the transcription task's
	// transcription_url points back at this fake, so the download the driver
	// performs itself is scripted like every other call.
	artifact func(path string) (int, string)
}

// artifactPathPrefix is where the fake serves scripted result files.
const artifactPathPrefix = "/artifacts/"

func (p *fakePlatform) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := recordedCall{
			method: r.Method,
			path:   r.URL.Path,
			auth:   r.Header.Get("Authorization"),
			async:  r.Header.Get("X-DashScope-Async"),
			sse:    r.Header.Get("X-DashScope-Sse"),
			work:   r.Header.Get("X-DashScope-WorkSpace"),
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&call.body)
		}
		p.mu.Lock()
		p.calls = append(p.calls, call)
		p.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, taskPathPrefix):
			p.answer(w, func(string) (int, string) {
				if p.query == nil {
					return http.StatusOK, "{}"
				}
				return p.query(strings.TrimPrefix(r.URL.Path, taskPathPrefix))
			}, "")
		case r.URL.Path == imagePathMultimodal:
			p.answer(w, func(string) (int, string) {
				if call.sse == "enable" && p.stream != nil {
					return p.stream(call.body)
				}
				responder := p.sync
				if responder == nil {
					responder = p.generate
				}
				if responder == nil {
					return http.StatusOK, "{}"
				}
				return responder(call.body)
			}, "")
		case r.URL.Path == generatePathText:
			p.answer(w, func(string) (int, string) {
				if call.sse == "enable" && p.stream != nil {
					return p.stream(call.body)
				}
				if p.generate == nil {
					return http.StatusOK, "{}"
				}
				return p.generate(call.body)
			}, "")
		case r.URL.Path == embedPathText:
			p.answer(w, func(string) (int, string) {
				if p.embed == nil {
					return http.StatusOK, "{}"
				}
				return p.embed(call.body)
			}, "")
		case strings.HasPrefix(r.URL.Path, artifactPathPrefix):
			p.answer(w, func(string) (int, string) {
				if p.artifact == nil {
					return http.StatusOK, "{}"
				}
				return p.artifact(r.URL.Path)
			}, "")
		default:
			p.answer(w, func(string) (int, string) {
				if p.create == nil {
					return http.StatusOK, "{}"
				}
				return p.create(call.body)
			}, "")
		}
	})
}

func (p *fakePlatform) answer(
	w http.ResponseWriter,
	responder func(string) (int, string),
	taskID string,
) {
	status, body := http.StatusOK, "{}"
	if responder != nil {
		status, body = responder(taskID)
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (p *fakePlatform) recorded() []recordedCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recordedCall(nil), p.calls...)
}

func (p *fakePlatform) lastCall(t *testing.T) recordedCall {
	t.Helper()
	calls := p.recorded()
	if len(calls) == 0 {
		t.Fatal("the platform saw no calls")
	}
	return calls[len(calls)-1]
}

// testClients starts a fake platform and returns a client rooted at it.
func testClients(t *testing.T, platform *fakePlatform) *mediaClient {
	t.Helper()
	server := httptest.NewServer(platform.handler())
	t.Cleanup(server.Close)
	platform.base = server.URL
	return newMediaClient("sk-test", server.URL, "", Spec{})
}

func TestTransportImageTask(t *testing.T) {
	platform := &fakePlatform{
		create: func(body map[string]any) (int, string) {
			if body["model"] != "wanx-v1" {
				t.Errorf("create model = %v", body["model"])
			}
			return http.StatusOK, `{"output":{"task_id":"task-1","task_status":"PENDING"},"request_id":"req-1"}`
		},
		query: func(taskID string) (int, string) {
			if taskID != "task-1" {
				t.Errorf("query task id = %q", taskID)
			}
			return http.StatusOK, `{"output":{"task_id":"task-1","task_status":"SUCCEEDED","results":[{"url":"https://cdn.example.com/a.png"},{"url":"https://cdn.example.com/b.png"}]},"usage":{"image_count":2},"request_id":"req-1"}`
		},
	}
	client := testClients(t, platform)

	handler := transportImage(client, time.Millisecond)
	raw, err := handler(context.Background(), imageWire{
		model:  "wanx-v1",
		prompt: "a kite",
		count:  2,
		format: media.ImageFormatPNG,
	})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if len(raw.items) != 2 || raw.items[0].url != "https://cdn.example.com/a.png" {
		t.Fatalf("items = %#v", raw.items)
	}
	if raw.requestID != "req-1" || raw.taskID != "task-1" {
		t.Fatalf("ids = %q/%q", raw.requestID, raw.taskID)
	}
	if raw.usage.imageCount != 2 {
		t.Errorf("usage = %+v", raw.usage)
	}

	calls := platform.recorded()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want the create and the query", len(calls))
	}
	if calls[0].async != "enable" {
		t.Errorf("create header X-DashScope-Async = %q, want enable", calls[0].async)
	}
	if calls[0].auth != "Bearer sk-test" {
		t.Errorf("authorization = %q", calls[0].auth)
	}
	if calls[1].async != "" {
		t.Errorf("query carried the async header: %#v", calls[1])
	}

	response, err := decodeImage(context.Background(), raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Message.Content.Parts) != 2 {
		t.Fatalf("parts = %#v", response.Message.Content.Parts)
	}
	if response.Usage.GeneratedImages == nil || *response.Usage.GeneratedImages != 2 {
		t.Errorf("usage = %+v", response.Usage)
	}
	if response.FinishReason != inference.FinishCompleted {
		t.Errorf("finish reason = %q", response.FinishReason)
	}
	if response.Metadata.RequestID != "req-1" {
		t.Errorf("metadata = %+v", response.Metadata)
	}
	part, ok := response.Message.Content.Parts[0].(message.ImagePart)
	if !ok {
		t.Fatalf("part = %#v", response.Message.Content.Parts[0])
	}
	if part.Source.MediaType() != "image/png" {
		t.Errorf("media type = %q", part.Source.MediaType())
	}
}

func TestTransportImageEditSurface(t *testing.T) {
	platform := &fakePlatform{
		create: func(map[string]any) (int, string) {
			return http.StatusOK, `{"output":{"task_id":"task-2","task_status":"PENDING"},"request_id":"req-2"}`
		},
		query: func(string) (int, string) {
			return http.StatusOK, `{"output":{"task_id":"task-2","task_status":"SUCCEEDED","results":[{"url":"https://cdn.example.com/edited.png"}]},"request_id":"req-2"}`
		},
	}
	client := testClients(t, platform)
	handler := transportImage(client, time.Millisecond)
	if _, err := handler(context.Background(), imageWire{
		model:    "wanx2.1-imageedit",
		edit:     true,
		prompt:   "make it snow",
		function: "description_edit_with_mask",
		images:   []string{"https://example.com/base.png"},
		count:    1,
		format:   media.ImageFormatPNG,
	}); err != nil {
		t.Fatalf("transport: %v", err)
	}
	call := platform.recorded()[0]
	if call.path != imagePathImage2Image {
		t.Errorf("create path = %q", call.path)
	}
	input := call.body["input"].(map[string]any)
	if input["function"] != "description_edit_with_mask" {
		t.Errorf("input = %#v", input)
	}
}

func TestTransportImageTaskRetentionUnknown(t *testing.T) {
	platform := &fakePlatform{
		create: func(map[string]any) (int, string) {
			return http.StatusOK, `{"output":{"task_id":"task-3","task_status":"PENDING"}}`
		},
		query: func(string) (int, string) {
			return http.StatusOK, `{"output":{"task_id":"task-3","task_status":"UNKNOWN"},"request_id":"req-3"}`
		},
	}
	client := testClients(t, platform)
	_, err := transportImage(client, time.Millisecond)(context.Background(), imageWire{
		model: "wanx-v1", prompt: "a kite", count: 1, format: media.ImageFormatPNG,
	})
	if err == nil {
		t.Fatal("transport accepted an unknown task")
	}
	if !errdefs.IsNotFound(err) {
		t.Errorf("error = %v, want not-found", err)
	}
	if !strings.Contains(err.Error(), "24 hours") {
		t.Errorf("error = %v, want the retention hint", err)
	}
}

func TestTransportImageTaskFailed(t *testing.T) {
	platform := &fakePlatform{
		create: func(map[string]any) (int, string) {
			return http.StatusOK, `{"output":{"task_id":"task-4","task_status":"PENDING"}}`
		},
		query: func(string) (int, string) {
			return http.StatusOK, `{"output":{"task_id":"task-4","task_status":"FAILED","code":"DataInspectionFailed","message":"inappropriate content"},"request_id":"req-4"}`
		},
	}
	client := testClients(t, platform)
	_, err := transportImage(client, time.Millisecond)(context.Background(), imageWire{
		model: "wanx-v1", prompt: "a kite", count: 1, format: media.ImageFormatPNG,
	})
	if err == nil {
		t.Fatal("transport accepted a failed task")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("error = %v, want validation", err)
	}
	if !strings.Contains(err.Error(), "inappropriate content") {
		t.Errorf("error = %v, want the platform message", err)
	}
}

func TestTransportImageCreateRejectsInBand(t *testing.T) {
	// The platform reports request-level failures on an HTTP 200 create
	// response, so the envelope has to be read rather than trusted.
	platform := &fakePlatform{
		create: func(map[string]any) (int, string) {
			return http.StatusOK, `{"code":"InvalidParameter","message":"size is invalid","request_id":"req-5"}`
		},
	}
	client := testClients(t, platform)
	_, err := transportImage(client, time.Millisecond)(context.Background(), imageWire{
		model: "wanx-v1", prompt: "a kite", count: 1, format: media.ImageFormatPNG,
	})
	if err == nil {
		t.Fatal("transport accepted an in-band create failure")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("error = %v, want validation", err)
	}
	if len(platform.recorded()) != 1 {
		t.Errorf("calls = %d, want only the create", len(platform.recorded()))
	}
}

func TestTransportImageSync(t *testing.T) {
	platform := &fakePlatform{
		sync: func(body map[string]any) (int, string) {
			if body["model"] != "wan2.6-image" {
				t.Errorf("model = %v", body["model"])
			}
			return http.StatusOK, `{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":[{"text":"here is your poster"},{"image":"https://cdn.example.com/poster.png"}]}}]},"usage":{"image_count":1},"request_id":"req-6"}`
		},
	}
	client := testClients(t, platform)
	raw, err := transportImage(client, time.Millisecond)(context.Background(), imageWire{
		model:            "wan2.6-image",
		sync:             true,
		prompt:           "a travel poster",
		count:            1,
		format:           media.ImageFormatPNG,
		enableInterleave: true,
	})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if len(raw.items) != 2 || raw.items[0].text != "here is your poster" ||
		raw.items[1].url != "https://cdn.example.com/poster.png" {
		t.Fatalf("items = %#v", raw.items)
	}
	call := platform.lastCall(t)
	if call.async != "" {
		t.Errorf("the sync call carried the async header: %#v", call)
	}
	if call.sse != "enable" {
		t.Errorf("the interleaved call carried X-DashScope-Sse %q, want enable", call.sse)
	}
	if call.path != imagePathMultimodal {
		t.Errorf("path = %q", call.path)
	}
	parameters := call.body["parameters"].(map[string]any)
	if parameters["enable_interleave"] != true {
		t.Errorf("parameters = %#v", parameters)
	}
	if parameters["stream"] != true {
		t.Errorf("parameters.stream = %v, want the flag the sse header pairs with",
			parameters["stream"])
	}

	response, err := decodeImage(context.Background(), raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Message.Content.Parts) != 2 {
		t.Fatalf("parts = %#v", response.Message.Content.Parts)
	}
	if _, ok := response.Message.Content.Parts[0].(message.TextPart); !ok {
		t.Errorf("first part = %#v, want the interleaved text", response.Message.Content.Parts[0])
	}
}

func TestTransportImageSyncInterleavedStream(t *testing.T) {
	// The interleaved surface streams the run: the driver has to ask for the
	// stream (the header is mandatory with enable_interleave) and fold the
	// events, text fragment by text fragment, into one result.
	stream := strings.Join([]string{
		`data: {"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":[{"type":"text","text":"肉香"}]}}]},"usage":{"image_count":2,"input_tokens":28},"request_id":"req-7"}`,
		"",
		":heartbeat",
		"",
		`data: {"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":[{"type":"text","text":"交织"},{"type":"image","image":"https://cdn.example.com/one.png"}]}}]},"usage":{"image_count":2,"output_tokens":545},"request_id":"req-7"}`,
		"",
		`data: {"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":[{"type":"image","image":"https://cdn.example.com/two.png"}]}}]},"usage":{"image_count":2},"request_id":"req-7"}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	platform := &fakePlatform{
		sync: func(map[string]any) (int, string) {
			return http.StatusOK, stream
		},
	}
	client := testClients(t, platform)
	raw, err := transportImage(client, time.Millisecond)(context.Background(), imageWire{
		model:            "wan2.6-image",
		sync:             true,
		prompt:           "a bowl of noodles",
		count:            2,
		format:           media.ImageFormatPNG,
		enableInterleave: true,
	})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if call := platform.lastCall(t); call.sse != "enable" {
		t.Errorf("X-DashScope-Sse = %q, want enable", call.sse)
	}
	if len(raw.items) != 3 {
		t.Fatalf("items = %#v", raw.items)
	}
	if raw.items[0].text != "肉香交织" {
		t.Errorf("folded text = %q", raw.items[0].text)
	}
	if raw.items[1].url != "https://cdn.example.com/one.png" ||
		raw.items[2].url != "https://cdn.example.com/two.png" {
		t.Errorf("images = %#v", raw.items[1:])
	}
	if raw.requestID != "req-7" || raw.usage.imageCount != 2 {
		t.Errorf("raw = %+v", raw)
	}
}

func TestTransportImageSyncOmitsSSE(t *testing.T) {
	// Only the interleaved surface streams; a plain sync call must leave the
	// header off so the endpoint answers with one object.
	platform := &fakePlatform{
		sync: func(map[string]any) (int, string) {
			return http.StatusOK, `{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":[{"type":"image","image":"https://cdn.example.com/edit.png"}]}}]},"usage":{"image_count":1},"request_id":"req-8"}`
		},
	}
	client := testClients(t, platform)
	if _, err := transportImage(client, time.Millisecond)(context.Background(), imageWire{
		model:  "wan2.6-image",
		sync:   true,
		prompt: "the same cat, at night",
		count:  1,
		format: media.ImageFormatPNG,
	}); err != nil {
		t.Fatalf("transport: %v", err)
	}
	if call := platform.lastCall(t); call.sse != "" {
		t.Errorf("X-DashScope-Sse = %q, want the header left off", call.sse)
	}
}

func TestTransportVideoTask(t *testing.T) {
	platform := &fakePlatform{
		create: func(body map[string]any) (int, string) {
			input := body["input"].(map[string]any)
			if input["first_frame_url"] != "https://example.com/start.png" {
				t.Errorf("input = %#v", input)
			}
			return http.StatusOK, `{"output":{"task_id":"task-7","task_status":"PENDING"},"request_id":"req-7"}`
		},
		query: func(string) (int, string) {
			// The platform reports durations as JSON floats; the driver
			// reads none of them, and a decode that insisted on integers
			// would fail the whole task.
			return http.StatusOK, `{"output":{"task_id":"task-7","task_status":"SUCCEEDED","video_url":"https://cdn.example.com/clip.mp4"},"usage":{"video_count":1,"duration":5.0,"input_video_duration":0.0,"output_video_duration":5.0},"request_id":"req-7"}`
		},
	}
	client := testClients(t, platform)
	raw, err := transportVideo(client, time.Millisecond)(context.Background(), videoWire{
		model:      "wanx2.1-kf2v-plus",
		keyframes:  true,
		prompt:     "the canoe drifts",
		firstFrame: "https://example.com/start.png",
	})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if raw.url != "https://cdn.example.com/clip.mp4" {
		t.Fatalf("url = %q", raw.url)
	}
	if raw.videoCount != 1 || raw.taskID != "task-7" {
		t.Errorf("raw = %+v", raw)
	}
	if path := platform.recorded()[0].path; path != videoPathImage2 {
		t.Errorf("create path = %q", path)
	}

	response, err := decodeVideo(context.Background(), raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	part, ok := response.Message.Content.Parts[0].(message.VideoPart)
	if !ok {
		t.Fatalf("part = %#v", response.Message.Content.Parts[0])
	}
	if part.Source.MediaType() != videoMediaType {
		t.Errorf("media type = %q", part.Source.MediaType())
	}
	if response.Usage.GeneratedVideos == nil || *response.Usage.GeneratedVideos != 1 {
		t.Errorf("usage = %+v", response.Usage)
	}
}

// TestTransportVideoMediaArray pins the wire the media surface produces: the
// synthesis endpoint (not the 2.x image2video one), input.media instead of the
// flat frame keys, and the -1 the smart-duration request sends.
func TestTransportVideoMediaArray(t *testing.T) {
	platform := &fakePlatform{
		create: func(body map[string]any) (int, string) {
			input := body["input"].(map[string]any)
			if _, ok := input["first_frame_url"]; ok {
				t.Errorf("the media wire also carried the 2.x frame key: %#v", input)
			}
			entries, ok := input["media"].([]any)
			if !ok || len(entries) != 1 {
				t.Fatalf("input.media = %#v", input["media"])
			}
			entry, _ := entries[0].(map[string]any)
			if entry["type"] != "first_frame" || entry["url"] != "https://example.com/start.png" {
				t.Errorf("media[0] = %#v", entry)
			}
			parameters := body["parameters"].(map[string]any)
			if parameters["duration"] != float64(-1) {
				t.Errorf("parameters.duration = %v, want the smart-duration -1", parameters["duration"])
			}
			return http.StatusOK, `{"output":{"task_id":"task-8","task_status":"PENDING"},"request_id":"req-8"}`
		},
		query: func(string) (int, string) {
			return http.StatusOK, `{"output":{"task_id":"task-8","task_status":"SUCCEEDED","video_url":"https://cdn.example.com/clip.mp4"},"usage":{"video_count":1},"request_id":"req-8"}`
		},
	}
	client := testClients(t, platform)
	raw, err := transportVideo(client, time.Millisecond)(context.Background(), videoWire{
		model:         "wan3.0-video",
		prompt:        "she opens the window",
		media:         []mediaEntry{{Type: mediaEntryFirstFrame, URL: "https://example.com/start.png"}},
		smartDuration: true,
	})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if raw.url != "https://cdn.example.com/clip.mp4" {
		t.Fatalf("url = %q", raw.url)
	}
	if path := platform.recorded()[0].path; path != videoPathSynthesis {
		t.Errorf("create path = %q, want the synthesis endpoint", path)
	}
}

func TestTransportHTTPFailuresClassify(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		requestID string
		check     func(error) bool
	}{
		{
			name:      "invalid key",
			status:    http.StatusUnauthorized,
			body:      `{"code":"InvalidApiKey","message":"Invalid API-key provided.","request_id":"req-8"}`,
			requestID: "req-8",
			check:     errdefs.IsUnauthorized,
		},
		{
			name:      "arrears",
			status:    http.StatusForbidden,
			body:      `{"code":"Arrearage","message":"Access denied, please make sure your account is in good standing.","request_id":"req-9"}`,
			requestID: "req-9",
			check:     errdefs.IsForbidden,
		},
		{
			name:      "throttled",
			status:    http.StatusTooManyRequests,
			body:      `{"code":"Throttling","message":"Requests throttled.","request_id":"req-10"}`,
			requestID: "req-10",
			check:     errdefs.IsRateLimit,
		},
		{
			name:      "unknown code falls back to the status",
			status:    http.StatusBadGateway,
			body:      `{"code":"SomethingNew","message":"boom","request_id":"req-11"}`,
			requestID: "req-11",
			check:     errdefs.IsNotAvailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			platform := &fakePlatform{
				create: func(map[string]any) (int, string) {
					return tc.status, tc.body
				},
			}
			client := testClients(t, platform)
			_, err := transportImage(client, time.Millisecond)(context.Background(), imageWire{
				model: "wanx-v1", prompt: "a kite", count: 1, format: media.ImageFormatPNG,
			})
			if err == nil {
				t.Fatal("transport accepted a failing create")
			}
			if !tc.check(err) {
				t.Errorf("error = %v, want the classified family", err)
			}
			// The request id rides the error chain rather than the message,
			// where telemetry reads it.
			if id, ok := errdefs.RequestID(err); !ok || id != tc.requestID {
				t.Errorf("RequestID(err) = %q, %v; want %q", id, ok, tc.requestID)
			}
		})
	}
}

func TestTransportWorkspaceHeader(t *testing.T) {
	platform := &fakePlatform{
		create: func(map[string]any) (int, string) {
			return http.StatusOK, `{"output":{"task_id":"task-12","task_status":"PENDING"}}`
		},
		query: func(string) (int, string) {
			return http.StatusOK, `{"output":{"task_id":"task-12","task_status":"SUCCEEDED","results":[{"url":"https://cdn.example.com/a.png"}]}}`
		},
	}
	server := httptest.NewServer(platform.handler())
	defer server.Close()
	client := newMediaClient("sk-test", server.URL, "ws-42", Spec{})

	if _, err := transportImage(client, time.Millisecond)(context.Background(), imageWire{
		model: "wanx-v1", prompt: "a kite", count: 1, format: media.ImageFormatPNG,
	}); err != nil {
		t.Fatalf("transport: %v", err)
	}
	if work := platform.recorded()[0].work; work != "ws-42" {
		t.Errorf("X-DashScope-WorkSpace = %q", work)
	}
}

func TestAwaitTaskPollsUntilTerminal(t *testing.T) {
	statuses := []string{"PENDING", "RUNNING", "SUCCEEDED"}
	index := 0
	platform := &fakePlatform{
		query: func(string) (int, string) {
			status := statuses[index]
			if index < len(statuses)-1 {
				index++
			}
			body := `{"output":{"task_id":"task-13","task_status":"` + status + `"}}`
			if status == "SUCCEEDED" {
				body = `{"output":{"task_id":"task-13","task_status":"SUCCEEDED","results":[{"url":"https://cdn.example.com/a.png"}]}}`
			}
			return http.StatusOK, body
		},
	}
	client := testClients(t, platform)
	query, err := awaitTask(context.Background(), client, time.Millisecond, "task-13", "image generation")
	if err != nil {
		t.Fatalf("awaitTask: %v", err)
	}
	if query.Output.TaskStatus != taskStatusSucceeded {
		t.Fatalf("status = %q", query.Output.TaskStatus)
	}
	if calls := len(platform.recorded()); calls != 3 {
		t.Errorf("query calls = %d, want one per status", calls)
	}
}

func TestAwaitTaskHonorsContext(t *testing.T) {
	platform := &fakePlatform{
		query: func(string) (int, string) {
			return http.StatusOK, `{"output":{"task_id":"task-14","task_status":"RUNNING"}}`
		},
	}
	client := testClients(t, platform)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := awaitTask(ctx, client, time.Millisecond, "task-14", "image generation")
	if err == nil {
		t.Fatal("awaitTask returned on a running task")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("error = %v, want the context error", err)
	}
}

func TestImageURLsPerEntryFailure(t *testing.T) {
	query := taskQuery{}
	query.Output.Results = append(query.Output.Results, taskResultItem{
		Code: "DataInspectionFailed", Message: "flagged",
	})
	if _, err := imageURLs(query); err == nil {
		t.Fatal("a wholly failed result list produced no error")
	} else if !strings.Contains(err.Error(), "flagged") {
		t.Errorf("error = %v, want the per-entry message", err)
	}

	query.Output.Results = append(query.Output.Results, taskResultItem{
		URL: "https://cdn.example.com/ok.png",
	})
	urls, err := imageURLs(query)
	if err != nil {
		t.Fatalf("imageURLs: %v", err)
	}
	if len(urls) != 1 || urls[0] != "https://cdn.example.com/ok.png" {
		t.Fatalf("urls = %#v", urls)
	}
}

func TestPollIntervalAndBaseURL(t *testing.T) {
	if got := (Spec{}).pollInterval(); got != defaultPollInterval {
		t.Errorf("pollInterval() = %v", got)
	}
	if got := (Spec{PollIntervalMillis: 1200}).pollInterval(); got != 1200*time.Millisecond {
		t.Errorf("pollInterval() = %v", got)
	}
	if got := (Spec{}).baseURL(); got != DefaultBaseURL {
		t.Errorf("baseURL() = %q", got)
	}
	if got := (Spec{BaseURL: "https://example.com/"}).baseURL(); got != "https://example.com" {
		t.Errorf("baseURL() = %q", got)
	}
}

func TestTransportImageSyncFailure(t *testing.T) {
	platform := &fakePlatform{
		sync: func(map[string]any) (int, string) {
			// The platform refuses an unknown model as InvalidParameter
			// ("Model not exist."); on the sync surface the refusal
			// arrives in band, under an HTTP 200.
			return http.StatusOK, `{"code":"InvalidParameter","message":"Model not exist.","request_id":"req-15"}`
		},
	}
	client := testClients(t, platform)
	_, err := transportImage(client, time.Millisecond)(context.Background(), imageWire{
		model: "wan2.6-image", sync: true, prompt: "a poster", count: 1,
		format: media.ImageFormatPNG,
	})
	if err == nil {
		t.Fatal("transport accepted an in-band sync failure")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("error = %v, want validation", err)
	}
}
