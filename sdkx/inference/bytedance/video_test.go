package bytedance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/media"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// newVideoTestRuntime builds a runtime whose task polls are paced at 1ms
// via the deployment Spec, keeping poll-loop tests fast.
func newVideoTestRuntime(t *testing.T, server *httptest.Server) *inference.Runtime {
	t.Helper()
	provider := buildProvider(t, map[string]any{
		"base_url":                   server.URL + "/api/v3",
		"video_poll_interval_millis": 1,
	}, speechProfiles())
	runtime, err := inference.NewRuntime([]inference.ProviderDefinition{provider})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return runtime
}

// videoTaskHandler serves the task API: POST creates the task, every GET
// polls it. succeedOnPoll selects which poll reports the terminal state.
func videoTaskHandler(
	polls *int,
	statuses []string,
	taskError map[string]any,
) func(w http.ResponseWriter, body map[string]any, _ bool) {
	return func(w http.ResponseWriter, body map[string]any, _ bool) {
		w.Header().Set("Content-Type", "application/json")
		if body == nil { // GET poll
			*polls++
			status := statuses[min(*polls-1, len(statuses)-1)]
			payload, _ := json.Marshal(map[string]any{
				"id":      "cgt-1",
				"status":  status,
				"error":   taskError,
				"content": map[string]any{"video_url": "https://example.com/out.mp4"},
				"usage":   map[string]any{"completion_tokens": 1320},
			})
			fmt.Fprint(w, string(payload))
			return
		}
		payload, _ := json.Marshal(map[string]any{"id": "cgt-1"})
		fmt.Fprint(w, string(payload))
	}
}

func videoRequest() inference.GenerateRequest {
	return inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: inference.Content{
					Parts: []inference.Part{inference.TextPart{Text: "waves at dusk"}},
				},
				Intent: inference.Intent{Video: &inference.VideoIntent{}},
			},
		},
	}
}

func TestVideoCapturedWire(t *testing.T) {
	polls := 0
	server, capture := newCapturedArk(t, videoTaskHandler(
		&polls, []string{"running", "succeeded"}, nil,
	))
	defer server.Close()
	runtime := newVideoTestRuntime(t, server)

	first, err := media.NewImageURL("https://example.com/first.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	last, err := media.NewImageURL("https://example.com/last.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	duration := int64(5000)
	seed := int64(42)
	watermark := true
	fixed := true
	audioTrack := true
	expires := int64(3600)
	request := videoRequest()
	request.Input.Content.Parts = append(
		request.Input.Content.Parts,
		inference.ImagePart{Source: first}, inference.ImagePart{Source: last},
	)
	request.Input.Content.Intent.Video = &inference.VideoIntent{
		DurationMillis: &duration,
		Resolution:     "720p",
		AspectRatio:    media.AspectRatio("16:9"),
		Seed:           &seed,
		Watermark:      &watermark,
	}
	request.Extensions = inference.Extensions{VideoOptions{
		CameraFixed:           &fixed,
		GenerateAudio:         &audioTrack,
		ServiceTier:           "flex",
		ExecutionExpiresAfter: &expires,
	}}
	response, err := runtime.Generate(
		context.Background(),
		generateModel("doubao-seedance-2-0"),
		request,
	)
	if err != nil {
		for depth, current := 0, err; current != nil && depth < 8; depth++ {
			t.Logf("depth %d: %T: %v", depth, current, current)
			current = errors.Unwrap(current)
		}
		t.Fatalf("Generate: %v", err)
	}
	if response.FinishReason != inference.FinishCompleted {
		t.Fatalf("finish = %q", response.FinishReason)
	}
	if len(response.Message.Content.Parts) != 1 {
		t.Fatalf("parts = %d", len(response.Message.Content.Parts))
	}
	part, ok := response.Message.Content.Parts[0].(inference.VideoPart)
	if !ok {
		t.Fatalf("part = %#v", response.Message.Content.Parts[0])
	}
	if part.Source.Kind() != media.SourceURL || part.Source.MediaType() != "video/mp4" {
		t.Fatalf("source = %+v", part.Source)
	}
	if response.Usage.OutputTokens != 1320 || response.Usage.TotalTokens != 1320 {
		t.Fatalf("usage = %+v", response.Usage)
	}
	if response.Usage.GeneratedVideos == nil || *response.Usage.GeneratedVideos != 1 {
		t.Fatalf("GeneratedVideos = %v", response.Usage.GeneratedVideos)
	}
	if polls == 0 {
		t.Fatal("task was never polled")
	}

	body := capture.body(0)
	if body["model"] != "doubao-seedance-2-0" {
		t.Fatalf("model = %v", body["model"])
	}
	items, ok := body["content"].([]any)
	if !ok || len(items) != 3 {
		t.Fatalf("content = %#v", body["content"])
	}
	textItem, _ := items[0].(map[string]any)
	if textItem["type"] != "text" || textItem["text"] != "waves at dusk" {
		t.Fatalf("text item = %#v", items[0])
	}
	firstItem, _ := items[1].(map[string]any)
	firstURL, _ := firstItem["image_url"].(map[string]any)
	if firstItem["type"] != "image_url" || firstItem["role"] != "first_frame" ||
		firstURL["url"] != "https://example.com/first.png" {
		t.Fatalf("first-frame item = %#v", items[1])
	}
	lastItem, _ := items[2].(map[string]any)
	if lastItem["role"] != "last_frame" {
		t.Fatalf("last-frame item = %#v", items[2])
	}
	for key, want := range map[string]any{
		"duration":                float64(5),
		"resolution":              "720p",
		"ratio":                   "16:9",
		"seed":                    float64(42),
		"watermark":               true,
		"camera_fixed":            true,
		"generate_audio":          true,
		"service_tier":            "flex",
		"execution_expires_after": float64(3600),
	} {
		if body[key] != want {
			t.Fatalf("%s = %v, want %v", key, body[key], want)
		}
	}
}

func TestVideoRejections(t *testing.T) {
	duration := int64(5500)
	video, err := media.NewVideoURL("https://example.com/in.mp4", "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	image, err := media.NewImageURL("https://example.com/frame.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		model  string
		mutate func(*inference.GenerateRequest)
		field  inference.FieldID
	}{
		{
			name: "sub-second duration",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Video.DurationMillis = &duration
			},
			field: inference.FieldGenerateIntentVideoDuration,
		},
		{
			name:  "resolution over model cap",
			model: "doubao-seedance-2-0-fast",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Video.Resolution = "1080p"
			},
			field: inference.FieldGenerateIntentVideoResolution,
		},
		{
			name: "video reference input",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Parts = append(
					r.Input.Content.Parts, inference.VideoPart{Source: video},
				)
			},
			field: inference.FieldGenerateInputVideo,
		},
		{
			name: "third reference image",
			mutate: func(r *inference.GenerateRequest) {
				for range 3 {
					r.Input.Content.Parts = append(
						r.Input.Content.Parts, inference.ImagePart{Source: image},
					)
				}
			},
			field: inference.FieldGenerateInputImage,
		},
		{
			name: "text intent",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Text = &inference.TextIntent{}
			},
			field: inference.FieldGenerateIntentText,
		},
		{
			name: "audio intent",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Audio = &inference.AudioIntent{
					Voice:  media.VoiceSpec{ID: "v"},
					Format: media.AudioFormat{Encoding: media.AudioEncodingMP3},
				}
			},
			field: inference.FieldGenerateIntentAudio,
		},
		{
			name: "tools intent",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Tools = &inference.ToolsIntent{
					Definitions: []tool.Definition{{
						Name:        "search",
						InputSchema: json.RawMessage(`{"type":"object"}`),
					}},
				}
			},
			field: inference.FieldGenerateIntentTools,
		},
		{
			name: "sampling intent",
			mutate: func(r *inference.GenerateRequest) {
				temperature := 0.5
				r.Input.Content.Intent.Sampling = &inference.SamplingIntent{
					Temperature: &temperature,
				}
			},
			field: inference.FieldGenerateIntentSampling,
		},
		{
			name: "reasoning intent",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Reasoning = &inference.ReasoningIntent{
					Effort: inference.ReasoningLow,
				}
			},
			field: inference.FieldGenerateIntentReasoning,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, capture := newCapturedArk(t, func(w http.ResponseWriter, _ map[string]any, _ bool) {
				t.Error("transport must not run after compiler rejection")
			})
			defer server.Close()
			runtime := newTestRuntime(t, server)
			request := videoRequest()
			tc.mutate(&request)
			model := tc.model
			if model == "" {
				model = "doubao-seedance-2-0"
			}
			_, err := runtime.Generate(
				context.Background(),
				generateModel(model),
				request,
			)
			if err == nil {
				t.Fatal("expected compiler rejection")
			}
			var inferenceErr *inference.Error
			if !errors.As(err, &inferenceErr) || inferenceErr.Field != tc.field {
				t.Fatalf("field = %v, want %s", err, tc.field)
			}
			if !inference.IsKind(err, inference.UnsupportedFeature) {
				t.Fatalf("kind = %v", err)
			}
			if len(capture.bodies) != 0 {
				t.Fatalf("transport ran %d times", len(capture.bodies))
			}
		})
	}
}

func TestVideoFailedTaskClassification(t *testing.T) {
	polls := 0
	server, _ := newCapturedArk(t, videoTaskHandler(
		&polls,
		[]string{"failed"},
		map[string]any{"code": "InvalidParameter", "message": "unsupported ratio"},
	))
	defer server.Close()
	runtime := newVideoTestRuntime(t, server)
	_, err := runtime.Generate(
		context.Background(),
		generateModel("doubao-seedance-2-0"),
		videoRequest(),
	)
	if err == nil {
		t.Fatal("failed task accepted")
	}
	if !errdefs.IsValidation(err) {
		t.Fatalf("classification = %v", err)
	}
}

func TestVideoCancelledTask(t *testing.T) {
	polls := 0
	server, _ := newCapturedArk(t, videoTaskHandler(&polls, []string{"cancelled"}, nil))
	defer server.Close()
	runtime := newVideoTestRuntime(t, server)
	_, err := runtime.Generate(
		context.Background(),
		generateModel("doubao-seedance-2-0"),
		videoRequest(),
	)
	if err == nil {
		t.Fatal("cancelled task accepted")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("classification = %v", err)
	}
}

func TestResolutionWithin(t *testing.T) {
	cases := []struct {
		resolution, cap string
		want            bool
	}{
		{"720p", "720p", true},
		{"480p", "720p", true},
		{"1080p", "720p", false},
		{"4k", "1080p", false},
		{"1080p", "4k", true},
		{"hd", "720p", false},
		{"720p", "wide", false},
	}
	for _, tc := range cases {
		if got := resolutionWithin(tc.resolution, tc.cap); got != tc.want {
			t.Errorf("resolutionWithin(%q, %q) = %v, want %v",
				tc.resolution, tc.cap, got, tc.want)
		}
	}
}

func TestSpecVideoPollInterval(t *testing.T) {
	if got := (Spec{}).videoPollInterval(); got != defaultVideoPollInterval {
		t.Fatalf("unset interval = %v, want %v", got, defaultVideoPollInterval)
	}
	millis := int64(250)
	if got := (Spec{VideoPollIntervalMillis: &millis}).videoPollInterval(); got != 250*time.Millisecond {
		t.Fatalf("configured interval = %v", got)
	}
	zero := int64(0)
	if err := (Spec{VideoPollIntervalMillis: &zero}).Validate(); err == nil {
		t.Fatal("non-positive poll interval accepted")
	}
	if err := (Spec{VideoPollIntervalMillis: &millis}).Validate(); err != nil {
		t.Fatalf("valid interval rejected: %v", err)
	}
}

func TestVideoOptionsContract(t *testing.T) {
	expires := int64(0)
	if err := (VideoOptions{ServiceTier: "priority"}).Validate(); err == nil {
		t.Fatal("unknown service tier accepted")
	}
	if err := (VideoOptions{ExecutionExpiresAfter: &expires}).Validate(); err == nil {
		t.Fatal("non-positive expiry accepted")
	}
	fixed, audioTrack := true, true
	ttl := int64(1800)
	options := VideoOptions{
		CameraFixed:           &fixed,
		GenerateAudio:         &audioTrack,
		ServiceTier:           "flex",
		ExecutionExpiresAfter: &ttl,
	}
	if err := options.Validate(); err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
	fields := options.ActiveFields()
	if len(fields) != 4 {
		t.Fatalf("ActiveFields = %v", fields)
	}
	clone := options.Clone().(VideoOptions)
	*clone.CameraFixed = false
	*clone.ExecutionExpiresAfter = 1
	if !*options.CameraFixed || *options.ExecutionExpiresAfter != 1800 {
		t.Fatal("Clone shared a pointer")
	}
}
