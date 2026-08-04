package minimax

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

// Video generation pipeline tests, run end to end through the runtime
// against the fake server: the async create → poll → retrieve dance, the
// model-bound duration/resolution matrix, and the i2v gating.

// newVideoRuntime paces task polling at 1ms so poll-loop tests stay fast.
func newVideoRuntime(t *testing.T, server *messagesServer) *inference.Runtime {
	t.Helper()
	provider := buildProvider(t, map[string]any{
		"base_url":                   server.URL,
		"video_poll_interval_millis": 1,
	})
	runtime, err := inference.NewRuntime([]inference.ProviderDefinition{provider})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return runtime
}

func videoGenerateRequest() inference.GenerateRequest {
	return inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{
					Parts: []message.Part{message.TextPart{Text: "waves at dusk"}},
				},
				Intent: inference.Intent{Video: &inference.VideoIntent{}},
			},
		},
	}
}

// videoTaskHandler answers the async task pipeline by call order: the POST
// (the only request carrying a body) creates the task, GET polls report
// the queued statuses in order, and the call after the last status
// retrieves the file. The driver only retrieves after a Success poll, so
// ordering alone tells polls from retrievals.
func videoTaskHandler(gets *int, statuses []string) func(w http.ResponseWriter, body map[string]any) {
	return func(w http.ResponseWriter, body map[string]any) {
		if body != nil {
			fmt.Fprint(w, `{"task_id":"task-1","base_resp":{"status_code":0,"status_msg":"success"}}`)
			return
		}
		*gets++
		if *gets <= len(statuses) {
			payload, _ := json.Marshal(map[string]any{
				"task_id":   "task-1",
				"status":    statuses[*gets-1],
				"file_id":   "file-1",
				"base_resp": map[string]any{"status_code": 0, "status_msg": "success"},
			})
			fmt.Fprint(w, string(payload))
			return
		}
		fmt.Fprint(w, `{"file":{"download_url":"https://example.com/out.mp4"},"base_resp":{"status_code":0,"status_msg":"success"}}`)
	}
}

func TestVideoUnaryCapturedWire(t *testing.T) {
	gets := 0
	server := newMessagesServer(t, videoTaskHandler(&gets, []string{"Processing", "Success"}))
	runtime := newVideoRuntime(t, server)

	duration := int64(6000)
	watermark := true
	request := videoGenerateRequest()
	request.Input.Content.Intent.Video = &inference.VideoIntent{
		DurationMillis: &duration,
		Resolution:     "1080p",
		Watermark:      &watermark,
	}
	response, err := runtime.Generate(
		context.Background(),
		minimaxModel("MiniMax-Hailuo-2.3"),
		request,
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.FinishReason != inference.FinishCompleted {
		t.Fatalf("finish = %q", response.FinishReason)
	}
	if len(response.Message.Content.Parts) != 1 {
		t.Fatalf("parts = %d", len(response.Message.Content.Parts))
	}
	part, ok := response.Message.Content.Parts[0].(message.VideoPart)
	if !ok {
		t.Fatalf("part = %#v", response.Message.Content.Parts[0])
	}
	if part.Source.Kind() != media.SourceURL || part.Source.MediaType() != "video/mp4" {
		t.Fatalf("source = %+v", part.Source)
	}
	if part.Source.URL() != "https://example.com/out.mp4" {
		t.Fatalf("url = %q", part.Source.URL())
	}
	if response.Usage.GeneratedVideos == nil || *response.Usage.GeneratedVideos != 1 {
		t.Fatalf("GeneratedVideos = %v", response.Usage.GeneratedVideos)
	}
	if server.requests() != 4 {
		t.Fatalf("requests = %d, want create + 2 polls + retrieval", server.requests())
	}
	if gets != 3 {
		t.Fatalf("gets = %d, want 2 polls + retrieval", gets)
	}

	body := server.body(t, 0)
	if body["model"] != "MiniMax-Hailuo-2.3" {
		t.Fatalf("model = %v", body["model"])
	}
	if body["prompt"] != "waves at dusk" {
		t.Fatalf("prompt = %v", body["prompt"])
	}
	if body["duration"] != float64(6) {
		t.Fatalf("duration = %v", body["duration"])
	}
	if body["resolution"] != "1080P" {
		t.Fatalf("resolution = %v", body["resolution"])
	}
	if body["aigc_watermark"] != true {
		t.Fatalf("aigc_watermark = %v", body["aigc_watermark"])
	}
	if _, exists := body["first_frame_image"]; exists {
		t.Fatalf("first_frame_image must stay unset: %v", body)
	}
}

// TestVideoFirstFrameOnWire asserts the i2v path: an image input part
// compiles to first_frame_image, and unset knobs stay off the wire.
func TestVideoFirstFrameOnWire(t *testing.T) {
	gets := 0
	server := newMessagesServer(t, videoTaskHandler(&gets, []string{"Success"}))
	runtime := newVideoRuntime(t, server)

	frame, err := media.NewImageURL("https://example.com/frame.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	request := videoGenerateRequest()
	request.Input.Content.Parts = append(
		request.Input.Content.Parts, message.ImagePart{Source: frame},
	)
	if _, err := runtime.Generate(
		context.Background(),
		minimaxModel("MiniMax-Hailuo-2.3-Fast"),
		request,
	); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if server.requests() != 3 {
		t.Fatalf("requests = %d, want create + poll + retrieval", server.requests())
	}

	body := server.body(t, 0)
	if body["model"] != "MiniMax-Hailuo-2.3-Fast" {
		t.Fatalf("model = %v", body["model"])
	}
	if body["first_frame_image"] != "https://example.com/frame.png" {
		t.Fatalf("first_frame_image = %v", body["first_frame_image"])
	}
	for _, key := range []string{"duration", "resolution", "aigc_watermark"} {
		if _, exists := body[key]; exists {
			t.Fatalf("%s must stay unset: %v", key, body)
		}
	}
}

func TestVideoRejections(t *testing.T) {
	duration10s := int64(10000)
	seed := int64(1)
	frame, err := media.NewImageURL("https://example.com/frame.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	cases := []struct {
		name   string
		model  string
		mutate func(*inference.GenerateRequest)
		field  inference.FieldID
		// kind overrides the expected error kind when the runtime cannot
		// surface the compiler's structured rejection as-is.
		kind inference.ErrorKind
	}{
		{
			name:  "10s on fast model",
			model: "MiniMax-Hailuo-2.3-Fast",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Parts = append(
					r.Input.Content.Parts, message.ImagePart{Source: frame},
				)
				r.Input.Content.Intent.Video.DurationMillis = &duration10s
			},
			field: inference.FieldGenerateIntentVideoDuration,
		},
		{
			name: "720p resolution tier",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Video.Resolution = "720p"
			},
			field: inference.FieldGenerateIntentVideoResolution,
		},
		{
			// No image part is active to reject on a frameless request,
			// so the rejection lands on the video intent.
			name:  "fast model without first frame",
			model: "MiniMax-Hailuo-2.3-Fast",
			mutate: func(r *inference.GenerateRequest) {
			},
			field: inference.FieldGenerateIntentVideo,
		},
		{
			name: "aspect ratio",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Video.AspectRatio = "16:9"
			},
			field: inference.FieldGenerateIntentVideoAspectRatio,
		},
		{
			name: "10s at 1080P",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Video.DurationMillis = &duration10s
				r.Input.Content.Intent.Video.Resolution = "1080p"
			},
			field: inference.FieldGenerateIntentVideoDuration,
		},
		{
			name: "seed",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Video.Seed = &seed
			},
			field: inference.FieldGenerateIntentVideoSeed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newMessagesServer(t, func(w http.ResponseWriter, _ map[string]any) {
				t.Error("transport must not run after compiler rejection")
			})
			runtime := newVideoRuntime(t, server)
			request := videoGenerateRequest()
			tc.mutate(&request)
			model := tc.model
			if model == "" {
				model = "MiniMax-Hailuo-2.3"
			}
			_, err := runtime.Generate(context.Background(), minimaxModel(model), request)
			if err == nil {
				t.Fatal("expected compiler rejection")
			}
			if tc.kind != "" {
				if !inference.IsKind(err, tc.kind) {
					t.Fatalf("kind = %v, want %s", err, tc.kind)
				}
			} else {
				var inferenceErr *inference.Error
				if !errors.As(err, &inferenceErr) || inferenceErr.Field != tc.field {
					t.Fatalf("field = %v, want %s", err, tc.field)
				}
				if !inference.IsKind(err, inference.UnsupportedFeature) {
					t.Fatalf("kind = %v", err)
				}
			}
			if server.requests() != 0 {
				t.Fatalf("transport ran %d times", server.requests())
			}
		})
	}
}

// TestVideoFailedTask asserts a server-side task failure surfaces as
// not-available; the capital-F status also exercises case-insensitivity.
func TestVideoFailedTask(t *testing.T) {
	gets := 0
	server := newMessagesServer(t, videoTaskHandler(&gets, []string{"Fail"}))
	runtime := newVideoRuntime(t, server)

	_, err := runtime.Generate(
		context.Background(),
		minimaxModel("MiniMax-Hailuo-2.3"),
		videoGenerateRequest(),
	)
	if err == nil {
		t.Fatal("failed task accepted")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("classification = %v", err)
	}
	if !inference.IsKind(err, inference.ProviderFailure) {
		t.Fatalf("kind = %v, want provider failure", err)
	}
}
