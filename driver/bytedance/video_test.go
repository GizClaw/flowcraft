package bytedance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
)

const videoTaskTestPath = "/contents/generations/tasks"

func compileVideoRequest(parts []message.Part, options VideoOptions) inference.GenerateRequest {
	var extensions inference.Extensions
	extensions = append(extensions, options)
	return inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: parts},
				Intent:  inference.Intent{Video: &inference.VideoIntent{}},
			},
		},
		Extensions: extensions,
	}
}

func compileVideoWire(
	t *testing.T,
	model string,
	request inference.GenerateRequest,
) (videoWire, inference.CompileReport, error) {
	t.Helper()
	entry, ok := catalog[model]
	if !ok {
		t.Fatalf("catalog model %q missing", model)
	}
	compiled, err := compileVideo("ep-test", entry)(
		context.Background(),
		inference.ModelRef{ID: inference.ModelID{Provider: driverID, Name: model}},
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		return videoWire{}, compiled.Report, err
	}
	return compiled.Wire, compiled.Report, nil
}

// videoOptionsField qualifies one VideoOptions extension field the same way
// the compiler does, so tests can assert the exact FieldID.
func videoOptionsField(name string) inference.FieldID {
	return inference.ExtensionField(name).Qualify(VideoOptions{})
}

func TestCompileVideoGatesParamsByModel(t *testing.T) {
	seed := int64(11)
	cameraFixed := true
	generateAudio := true
	oneSecond := int64(1_000)
	thirteenSeconds := int64(13_000)
	overMaxSeed := int64(2_147_483_648)

	cases := []struct {
		name    string
		model   string
		options VideoOptions
		intent  func(*inference.VideoIntent)
		field   inference.FieldID
		reason  string
	}{
		{
			name:   "seed unsupported on 2.0",
			model:  "doubao-seedance-2-0",
			intent: func(i *inference.VideoIntent) { i.Seed = &seed },
			field:  inference.FieldGenerateIntentVideoSeed,
			reason: "does not support seed",
		},
		{
			name:    "camera_fixed unsupported on 2.0",
			model:   "doubao-seedance-2-0",
			options: VideoOptions{CameraFixed: &cameraFixed},
			field:   videoOptionsField("camera_fixed"),
			reason:  "does not support camera_fixed",
		},
		{
			name:    "generate_audio unsupported on 1.0 pro",
			model:   "doubao-seedance-1-0-pro",
			options: VideoOptions{GenerateAudio: &generateAudio},
			field:   videoOptionsField("generate_audio"),
			reason:  "does not support generate_audio",
		},
		{
			name:    "flex tier unsupported on 2.0",
			model:   "doubao-seedance-2-0",
			options: VideoOptions{ServiceTier: "flex"},
			field:   videoOptionsField("service_tier"),
			reason:  "does not support service_tier=flex",
		},
		{
			name:   "duration below 1.0 pro minimum",
			model:  "doubao-seedance-1-0-pro",
			intent: func(i *inference.VideoIntent) { i.DurationMillis = &oneSecond },
			field:  inference.FieldGenerateIntentVideoDuration,
			reason: "requires a duration of at least 2s",
		},
		{
			name:   "duration above 1.0 pro maximum",
			model:  "doubao-seedance-1-0-pro",
			intent: func(i *inference.VideoIntent) { i.DurationMillis = &thirteenSeconds },
			field:  inference.FieldGenerateIntentVideoDuration,
			reason: "caps duration at 12s",
		},
		{
			name:   "invalid ratio",
			model:  "doubao-seedance-2-0",
			intent: func(i *inference.VideoIntent) { i.AspectRatio = "2:1" },
			field:  inference.FieldGenerateIntentVideoAspectRatio,
			reason: `unsupported video ratio "2:1"`,
		},
		{
			name:   "seed above documented range on 1.5 pro",
			model:  "doubao-seedance-1-5-pro",
			intent: func(i *inference.VideoIntent) { i.Seed = &overMaxSeed },
			field:  inference.FieldGenerateIntentVideoSeed,
			reason: "seed must be within [-1, 2147483647]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := compileVideoRequest(
				[]message.Part{message.TextPart{Text: "a cinematic scene"}},
				tc.options,
			)
			if tc.intent != nil {
				tc.intent(request.Input.Content.Intent.Video)
			}
			_, report, err := compileVideoWire(t, tc.model, request)
			if err == nil {
				t.Fatalf("compile unexpectedly succeeded; report = %+v", report)
			}
			reason := rejectedReason(report, tc.field)
			if reason == "" {
				t.Fatalf("field %q not rejected; report = %+v", tc.field, report)
			}
			if !strings.Contains(reason, tc.reason) {
				t.Errorf("rejection reason = %q, want substring %q", reason, tc.reason)
			}
		})
	}
}

func TestCompileVideoAllowsSupportedParams(t *testing.T) {
	seed := int64(11)
	cameraFixed := true
	generateAudio := true
	fiveSeconds := int64(5_000)
	request := compileVideoRequest(
		[]message.Part{message.TextPart{Text: "a cinematic scene"}},
		VideoOptions{
			CameraFixed:   &cameraFixed,
			GenerateAudio: &generateAudio,
			ServiceTier:   "flex",
		},
	)
	video := request.Input.Content.Intent.Video
	video.DurationMillis = &fiveSeconds
	video.AspectRatio = "16:9"
	video.Seed = &seed

	wire, report, err := compileVideoWire(t, "doubao-seedance-1-5-pro", request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	if wire.duration == nil || *wire.duration != 5 {
		t.Fatalf("wire.duration = %v, want 5", wire.duration)
	}
	if wire.seed == nil || *wire.seed != seed {
		t.Fatalf("wire.seed = %v, want %d", wire.seed, seed)
	}
	if wire.cameraFixed == nil || !*wire.cameraFixed {
		t.Fatal("wire.cameraFixed not set")
	}
	if wire.generateAudio == nil || !*wire.generateAudio {
		t.Fatal("wire.generateAudio not set")
	}
	if wire.serviceTier != "flex" {
		t.Fatalf("wire.serviceTier = %q, want flex", wire.serviceTier)
	}
}

func TestVideoOptionsValidateExecutionExpiresAfter(t *testing.T) {
	for _, value := range []int64{600, 259_201} {
		if err := (VideoOptions{ExecutionExpiresAfter: &value}).Validate(); err == nil {
			t.Errorf("execution_expires_after=%d: expected validation error", value)
		}
	}
	for _, value := range []int64{3600, 172_800, 259_200} {
		if err := (VideoOptions{ExecutionExpiresAfter: &value}).Validate(); err != nil {
			t.Errorf("execution_expires_after=%d: unexpected error %v", value, err)
		}
	}
}

func TestCatalogVideoParamsMatchOfficialMatrix(t *testing.T) {
	// Transcription guard: catalogEntry.video mirrors the official
	// create-task API per-model support columns. Any drift here means the
	// compiler gates the wrong model set.
	checks := map[string]videoParams{
		"doubao-seedance-2-5": {
			generateAudio:  true,
			durationMin:    videoSeconds(4),
			durationMax:    videoSeconds(30),
			durationAuto:   true,
			referenceImage: 30,
			referenceVideo: 10,
			referenceAudio: 10,
		},
		"doubao-seedance-2-0": {
			generateAudio:  true,
			durationMin:    videoSeconds(4),
			durationMax:    videoSeconds(15),
			durationAuto:   true,
			referenceImage: 9,
			referenceVideo: 3,
			referenceAudio: 3,
		},
		"doubao-seedance-2-0-fast": {
			generateAudio:  true,
			durationMin:    videoSeconds(4),
			durationMax:    videoSeconds(15),
			durationAuto:   true,
			referenceImage: 9,
			referenceVideo: 3,
			referenceAudio: 3,
		},
		"doubao-seedance-1-5-pro": {
			seed:          true,
			cameraFixed:   true,
			flexTier:      true,
			generateAudio: true,
			durationMin:   videoSeconds(4),
			durationMax:   videoSeconds(12),
			durationAuto:  true,
		},
		"doubao-seedance-1-0-pro": {
			seed:        true,
			cameraFixed: true,
			flexTier:    true,
			durationMin: videoSeconds(2),
			durationMax: videoSeconds(12),
		},
	}
	for name, want := range checks {
		if got := catalog[name].video; !reflect.DeepEqual(got, want) {
			t.Errorf("model %q: video params = %+v, want %+v", name, got, want)
		}
	}
}

func TestTransportVideoExpired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == videoTaskTestPath:
			_, _ = writer.Write([]byte(`{"id":"cgt-test"}`))
		case request.Method == http.MethodGet && request.URL.Path == videoTaskTestPath+"/cgt-test":
			_, _ = writer.Write([]byte(`{"id":"cgt-test","status":"expired"}`))
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()

	client := arkruntime.NewClientWithApiKey(
		"sk-test",
		arkruntime.WithBaseUrl(server.URL),
		arkruntime.WithHTTPClient(server.Client()),
		arkruntime.WithRetryTimes(0),
	)
	_, err := transportVideo(client, time.Millisecond)(context.Background(), videoWire{
		model:  "ep-test",
		prompt: "a cinematic scene",
	})
	if err == nil {
		t.Fatal("transport returned nil error for expired task")
	}
	if !errdefs.IsTimeout(err) {
		t.Fatalf("expired task error = %v, want timeout classification", err)
	}
}

func TestTransportVideoSucceeded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == videoTaskTestPath:
			_, _ = writer.Write([]byte(`{"id":"cgt-test"}`))
		case request.Method == http.MethodGet && request.URL.Path == videoTaskTestPath+"/cgt-test":
			_, _ = writer.Write([]byte(`{
				"id": "cgt-test",
				"status": "succeeded",
				"content": {"video_url": "https://example.com/out.mp4"},
				"usage": {"completion_tokens": 12345}
			}`))
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()

	client := arkruntime.NewClientWithApiKey(
		"sk-test",
		arkruntime.WithBaseUrl(server.URL),
		arkruntime.WithHTTPClient(server.Client()),
		arkruntime.WithRetryTimes(0),
	)
	raw, err := transportVideo(client, time.Millisecond)(context.Background(), videoWire{
		model:  "ep-test",
		prompt: "a cinematic scene",
	})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if raw.videoURL != "https://example.com/out.mp4" {
		t.Fatalf("videoURL = %q", raw.videoURL)
	}
	if raw.completionTokens != 12345 {
		t.Fatalf("completionTokens = %d, want 12345", raw.completionTokens)
	}
}

func videoImagePart(t *testing.T, url string) message.ImagePart {
	t.Helper()
	source, err := media.NewImageURL(url, "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	return message.ImagePart{Source: source}
}

func videoReferenceVideoPart(t *testing.T, url string) message.VideoPart {
	t.Helper()
	source, err := media.NewVideoURL(url, "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoURL: %v", err)
	}
	return message.VideoPart{Source: source}
}

func videoReferenceAudioPart(t *testing.T, url string) message.AudioPart {
	t.Helper()
	source, err := media.NewAudioURL(url, "audio/mpeg")
	if err != nil {
		t.Fatalf("NewAudioURL: %v", err)
	}
	return message.AudioPart{Source: source}
}

func TestCompileVideoReferenceInputs(t *testing.T) {
	parts := func(kinds ...message.Part) []message.Part { return kinds }

	accepted := []struct {
		name       string
		model      string
		parts      []message.Part
		wantFirst  string
		wantLast   string
		wantImages []string
		wantVideos []string
		wantAudios []string
	}{
		{
			name:      "first frame",
			model:     "doubao-seedance-2-0",
			parts:     parts(videoImagePart(t, "https://example.com/a.png")),
			wantFirst: "https://example.com/a.png",
		},
		{
			name:  "first and last frames",
			model: "doubao-seedance-2-0",
			parts: parts(
				videoImagePart(t, "https://example.com/a.png"),
				videoImagePart(t, "https://example.com/b.png"),
			),
			wantFirst: "https://example.com/a.png",
			wantLast:  "https://example.com/b.png",
		},
		{
			name:  "reference images",
			model: "doubao-seedance-2-0",
			parts: parts(
				videoImagePart(t, "https://example.com/a.png"),
				videoImagePart(t, "https://example.com/b.png"),
				videoImagePart(t, "https://example.com/c.png"),
			),
			wantImages: []string{
				"https://example.com/a.png",
				"https://example.com/b.png",
				"https://example.com/c.png",
			},
		},
		{
			name:       "reference video",
			model:      "doubao-seedance-2-0",
			parts:      parts(videoReferenceVideoPart(t, "https://example.com/clip.mp4")),
			wantVideos: []string{"https://example.com/clip.mp4"},
		},
		{
			name:       "reference audio",
			model:      "doubao-seedance-2-0",
			parts:      parts(videoReferenceAudioPart(t, "https://example.com/track.mp3")),
			wantAudios: []string{"https://example.com/track.mp3"},
		},
		{
			name:       "audio only on 2.5",
			model:      "doubao-seedance-2-5",
			parts:      parts(videoReferenceAudioPart(t, "https://example.com/track.mp3")),
			wantAudios: []string{"https://example.com/track.mp3"},
		},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			wire, report, err := compileVideoWire(
				t,
				tc.model,
				compileVideoRequest(tc.parts, VideoOptions{}),
			)
			if err != nil {
				t.Fatalf("compile: %v; report = %+v", err, report)
			}
			if wire.firstFrame != tc.wantFirst {
				t.Errorf("firstFrame = %q, want %q", wire.firstFrame, tc.wantFirst)
			}
			if wire.lastFrame != tc.wantLast {
				t.Errorf("lastFrame = %q, want %q", wire.lastFrame, tc.wantLast)
			}
			if !reflect.DeepEqual(wire.referenceImages, tc.wantImages) {
				t.Errorf("referenceImages = %v, want %v", wire.referenceImages, tc.wantImages)
			}
			if !reflect.DeepEqual(wire.referenceVideos, tc.wantVideos) {
				t.Errorf("referenceVideos = %v, want %v", wire.referenceVideos, tc.wantVideos)
			}
			if !reflect.DeepEqual(wire.referenceAudios, tc.wantAudios) {
				t.Errorf("referenceAudios = %v, want %v", wire.referenceAudios, tc.wantAudios)
			}
		})
	}
}

func TestCompileVideoRejectsReferenceConflicts(t *testing.T) {
	tenImages := make([]message.Part, 0, 10)
	for i := range 10 {
		tenImages = append(tenImages, videoImagePart(
			t,
			"https://example.com/"+string(rune('a'+i))+".png",
		))
	}
	cases := []struct {
		name   string
		model  string
		parts  []message.Part
		field  inference.FieldID
		reason string
	}{
		{
			name:   "video reference unsupported on 1.5 pro",
			model:  "doubao-seedance-1-5-pro",
			parts:  []message.Part{videoReferenceVideoPart(t, "https://example.com/clip.mp4")},
			field:  inference.FieldGenerateInputVideo,
			reason: "does not support video-reference input",
		},
		{
			name:  "reference images unsupported on 1.5 pro",
			model: "doubao-seedance-1-5-pro",
			parts: []message.Part{
				videoImagePart(t, "https://example.com/a.png"),
				videoImagePart(t, "https://example.com/b.png"),
				videoImagePart(t, "https://example.com/c.png"),
			},
			field:  inference.FieldGenerateInputImage,
			reason: "does not support reference-image input",
		},
		{
			name:  "first frame and reference video conflict",
			model: "doubao-seedance-2-0",
			parts: []message.Part{
				videoImagePart(t, "https://example.com/a.png"),
				videoReferenceVideoPart(t, "https://example.com/clip.mp4"),
			},
			field:  inference.FieldGenerateInputImage,
			reason: "mutually exclusive",
		},
		{
			name:   "reference image count cap on 2.0",
			model:  "doubao-seedance-2-0",
			parts:  tenImages,
			field:  inference.FieldGenerateInputImage,
			reason: "supports at most 9 reference images",
		},
		{
			name:  "reference video count cap on 2.0",
			model: "doubao-seedance-2-0",
			parts: []message.Part{
				videoReferenceVideoPart(t, "https://example.com/1.mp4"),
				videoReferenceVideoPart(t, "https://example.com/2.mp4"),
				videoReferenceVideoPart(t, "https://example.com/3.mp4"),
				videoReferenceVideoPart(t, "https://example.com/4.mp4"),
			},
			field:  inference.FieldGenerateInputVideo,
			reason: "supports at most 3 reference videos",
		},
		{
			name:  "reference audio count cap on 2.0",
			model: "doubao-seedance-2-0",
			parts: []message.Part{
				videoReferenceAudioPart(t, "https://example.com/1.mp3"),
				videoReferenceAudioPart(t, "https://example.com/2.mp3"),
				videoReferenceAudioPart(t, "https://example.com/3.mp3"),
				videoReferenceAudioPart(t, "https://example.com/4.mp3"),
			},
			field:  inference.FieldGenerateInputAudio,
			reason: "supports at most 3 reference audio clips",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, report, err := compileVideoWire(
				t,
				tc.model,
				compileVideoRequest(tc.parts, VideoOptions{}),
			)
			if err == nil {
				t.Fatalf("compile unexpectedly succeeded; report = %+v", report)
			}
			reason := rejectedReason(report, tc.field)
			if reason == "" {
				t.Fatalf("field %q not rejected; report = %+v", tc.field, report)
			}
			if !strings.Contains(reason, tc.reason) {
				t.Errorf("rejection reason = %q, want substring %q", reason, tc.reason)
			}
		})
	}
}

func TestTransportVideoCarriesReferenceInputs(t *testing.T) {
	var body struct {
		Content []map[string]any `json:"content"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == videoTaskTestPath:
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
			}
			_, _ = writer.Write([]byte(`{"id":"cgt-test"}`))
		case request.Method == http.MethodGet && request.URL.Path == videoTaskTestPath+"/cgt-test":
			_, _ = writer.Write([]byte(`{
				"id": "cgt-test",
				"status": "succeeded",
				"content": {"video_url": "https://example.com/out.mp4"},
				"usage": {"completion_tokens": 1}
			}`))
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()

	client := arkruntime.NewClientWithApiKey(
		"sk-test",
		arkruntime.WithBaseUrl(server.URL),
		arkruntime.WithHTTPClient(server.Client()),
		arkruntime.WithRetryTimes(0),
	)
	_, err := transportVideo(client, time.Millisecond)(context.Background(), videoWire{
		model:           "ep-test",
		prompt:          "reference please",
		firstFrame:      "https://example.com/first.png",
		lastFrame:       "https://example.com/last.png",
		referenceImages: []string{"https://example.com/ref1.png"},
		referenceVideos: []string{"https://example.com/clip.mp4"},
		referenceAudios: []string{"https://example.com/track.mp3"},
	})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	want := []map[string]any{
		{"type": "text", "text": "reference please"},
		{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/first.png"}, "role": "first_frame"},
		{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/last.png"}, "role": "last_frame"},
		{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/ref1.png"}, "role": "reference_image"},
		{"type": "video_url", "video_url": map[string]any{"url": "https://example.com/clip.mp4"}, "role": "reference_video"},
		{"type": "audio_url", "audio_url": map[string]any{"url": "https://example.com/track.mp3"}, "role": "reference_audio"},
	}
	if !reflect.DeepEqual(body.Content, want) {
		t.Errorf("content = %#v, want %#v", body.Content, want)
	}
}
