package minimax

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func compileVideoRequest(
	parts []message.Part,
	options VideoOptions,
) inference.GenerateRequest {
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

func rejectedReason(report inference.CompileReport, field inference.FieldID) string {
	for _, decision := range report.Decisions {
		if decision.Field == field && decision.Disposition == inference.Rejected {
			return decision.Reason
		}
	}
	return ""
}

func videoOptionsField(name string) inference.FieldID {
	return inference.ExtensionField(name).Qualify(VideoOptions{})
}

func videoImagePart(t *testing.T, url string) message.ImagePart {
	t.Helper()
	source, err := media.NewImageURL(url, "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	return message.ImagePart{Source: source}
}

func videoInlineImagePart(t *testing.T, data []byte) message.ImagePart {
	t.Helper()
	source, err := media.NewImageBytes(data, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	return message.ImagePart{Source: source}
}

func videoClipPart(t *testing.T, url string) message.VideoPart {
	t.Helper()
	source, err := media.NewVideoURL(url, "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoURL: %v", err)
	}
	return message.VideoPart{Source: source}
}

func videoAudioPart(t *testing.T, url string) message.AudioPart {
	t.Helper()
	source, err := media.NewAudioURL(url, "audio/mpeg")
	if err != nil {
		t.Fatalf("NewAudioURL: %v", err)
	}
	return message.AudioPart{Source: source}
}

func TestCompileVideoV2Defaults(t *testing.T) {
	request := compileVideoRequest(
		[]message.Part{message.TextPart{Text: "a boy playing basketball"}},
		VideoOptions{},
	)
	wire, report, err := compileVideoWire(t, "MiniMax-H3", request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	if wire.duration == nil || *wire.duration != 6 {
		t.Fatalf("wire.duration = %v, want 6", wire.duration)
	}
	if wire.resolution != "768P" {
		t.Fatalf("wire.resolution = %q, want 768P", wire.resolution)
	}
	if wire.ratio != "16:9" {
		t.Fatalf("wire.ratio = %q, want 16:9", wire.ratio)
	}
	if wire.prompt != "a boy playing basketball" {
		t.Fatalf("wire.prompt = %q", wire.prompt)
	}
}

func TestCompileVideoV2GatesControls(t *testing.T) {
	threeSeconds := int64(3_000)
	sixteenSeconds := int64(16_000)
	seed := int64(11)
	optimizer := true
	pretreatment := true

	cases := []struct {
		name    string
		options VideoOptions
		intent  func(*inference.VideoIntent)
		field   inference.FieldID
		reason  string
	}{
		{
			name:   "duration below minimum",
			intent: func(i *inference.VideoIntent) { i.DurationMillis = &threeSeconds },
			field:  inference.FieldGenerateIntentVideoDuration,
			reason: "4-15s",
		},
		{
			name:   "duration above maximum",
			intent: func(i *inference.VideoIntent) { i.DurationMillis = &sixteenSeconds },
			field:  inference.FieldGenerateIntentVideoDuration,
			reason: "4-15s",
		},
		{
			name:   "resolution 1080P unsupported",
			intent: func(i *inference.VideoIntent) { i.Resolution = "1080P" },
			field:  inference.FieldGenerateIntentVideoResolution,
			reason: "768P/2K",
		},
		{
			name:   "ratio outside official set",
			intent: func(i *inference.VideoIntent) { i.AspectRatio = "2:1" },
			field:  inference.FieldGenerateIntentVideoAspectRatio,
			reason: "ratios are adaptive",
		},
		{
			name:   "ratio adaptive on text-only task",
			intent: func(i *inference.VideoIntent) { i.AspectRatio = "adaptive" },
			field:  inference.FieldGenerateIntentVideoAspectRatio,
			reason: "explicit ratio",
		},
		{
			name:   "seed unsupported",
			intent: func(i *inference.VideoIntent) { i.Seed = &seed },
			field:  inference.FieldGenerateIntentVideoSeed,
			reason: "no seed control",
		},
		{
			name:    "prompt_optimizer is v1-only",
			options: VideoOptions{PromptOptimizer: &optimizer},
			field:   videoOptionsField("prompt_optimizer"),
			reason:  "no prompt optimizer",
		},
		{
			name:    "fast_pretreatment is v1-only",
			options: VideoOptions{FastPretreatment: &pretreatment},
			field:   videoOptionsField("fast_pretreatment"),
			reason:  "no prompt optimizer",
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
			_, report, err := compileVideoWire(t, "MiniMax-H3", request)
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

func TestCompileVideoV2AcceptsControls(t *testing.T) {
	fifteenSeconds := int64(15_000)
	watermark := false
	request := compileVideoRequest(
		[]message.Part{message.TextPart{Text: "a cinematic scene"}},
		VideoOptions{CallbackURL: "https://example.com/callback"},
	)
	video := request.Input.Content.Intent.Video
	video.DurationMillis = &fifteenSeconds
	video.Resolution = "2k"
	video.AspectRatio = "21:9"
	video.Watermark = &watermark

	wire, report, err := compileVideoWire(t, "MiniMax-H3", request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	if wire.duration == nil || *wire.duration != 15 {
		t.Fatalf("wire.duration = %v, want 15", wire.duration)
	}
	if wire.resolution != "2K" {
		t.Fatalf("wire.resolution = %q, want 2K", wire.resolution)
	}
	if wire.ratio != "21:9" {
		t.Fatalf("wire.ratio = %q, want 21:9", wire.ratio)
	}
	if wire.watermark == nil || *wire.watermark {
		t.Fatalf("wire.watermark = %v, want false", wire.watermark)
	}
	if wire.callbackURL != "https://example.com/callback" {
		t.Fatalf("wire.callbackURL = %q", wire.callbackURL)
	}
}

func TestCompileVideoV2Inputs(t *testing.T) {
	url1 := "https://example.com/a.png"
	url2 := "https://example.com/b.png"
	url3 := "https://example.com/c.png"
	clip := "https://example.com/clip.mp4"
	audio := "https://example.com/track.mp3"

	accepted := []struct {
		name  string
		parts []message.Part
		check func(*testing.T, videoWire)
	}{
		{
			name:  "single image is the first frame",
			parts: []message.Part{videoImagePart(t, url1)},
			check: func(t *testing.T, wire videoWire) {
				if wire.firstFrame != url1 || wire.lastFrame != "" {
					t.Fatalf("frames = %q / %q, want first %q", wire.firstFrame, wire.lastFrame, url1)
				}
			},
		},
		{
			name:  "two images are first and last frame",
			parts: []message.Part{videoImagePart(t, url1), videoImagePart(t, url2)},
			check: func(t *testing.T, wire videoWire) {
				if wire.firstFrame != url1 || wire.lastFrame != url2 {
					t.Fatalf("frames = %q / %q", wire.firstFrame, wire.lastFrame)
				}
			},
		},
		{
			name:  "three images become reference images",
			parts: []message.Part{videoImagePart(t, url1), videoImagePart(t, url2), videoImagePart(t, url3)},
			check: func(t *testing.T, wire videoWire) {
				if len(wire.referenceImages) != 3 || wire.firstFrame != "" {
					t.Fatalf("referenceImages = %v, firstFrame = %q", wire.referenceImages, wire.firstFrame)
				}
			},
		},
		{
			name: "reference images, videos, and audios",
			parts: []message.Part{
				videoImagePart(t, url1), videoImagePart(t, url2), videoImagePart(t, url3),
				videoClipPart(t, clip), videoAudioPart(t, audio),
			},
			check: func(t *testing.T, wire videoWire) {
				if len(wire.referenceImages) != 3 ||
					len(wire.referenceVideos) != 1 ||
					len(wire.referenceAudios) != 1 {
					t.Fatalf("references = %v / %v / %v",
						wire.referenceImages, wire.referenceVideos, wire.referenceAudios)
				}
			},
		},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			wire, report, err := compileVideoWire(
				t,
				"MiniMax-H3",
				compileVideoRequest(
					append([]message.Part{message.TextPart{Text: "a scene"}}, tc.parts...),
					VideoOptions{},
				),
			)
			if err != nil {
				t.Fatalf("compile: %v; report = %+v", err, report)
			}
			tc.check(t, wire)
		})
	}

	rejected := []struct {
		name   string
		parts  []message.Part
		field  inference.FieldID
		reason string
	}{
		{
			name:   "first/last frame mixed with reference inputs",
			parts:  []message.Part{videoImagePart(t, url1), videoImagePart(t, url2), videoClipPart(t, clip)},
			field:  inference.FieldGenerateInputImage,
			reason: "mutually exclusive",
		},
		{
			name: "too many reference images",
			parts: func() []message.Part {
				parts := make([]message.Part, 0, 10)
				for i := 0; i < 10; i++ {
					parts = append(parts, videoImagePart(t, "https://example.com/x.png"))
				}
				return parts
			}(),
			field:  inference.FieldGenerateInputImage,
			reason: "at most 9",
		},
		{
			name: "too many reference videos",
			parts: func() []message.Part {
				parts := make([]message.Part, 0, 5)
				for i := 0; i < 4; i++ {
					parts = append(parts, videoClipPart(t, "https://example.com/clip.mp4"))
				}
				return parts
			}(),
			field:  inference.FieldGenerateInputVideo,
			reason: "at most 3 reference videos",
		},
		{
			name: "too many reference audios",
			parts: func() []message.Part {
				parts := make([]message.Part, 0, 5)
				for i := 0; i < 4; i++ {
					parts = append(parts, videoAudioPart(t, "https://example.com/track.mp3"))
				}
				return parts
			}(),
			field:  inference.FieldGenerateInputAudio,
			reason: "at most 3 reference audio",
		},
		{
			name:   "missing text prompt",
			parts:  nil,
			field:  inference.FieldGenerateIntentVideo,
			reason: "non-empty text prompt",
		},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			parts := tc.parts
			if parts == nil {
				parts = []message.Part{}
			}
			_, report, err := compileVideoWire(
				t,
				"MiniMax-H3",
				compileVideoRequest(parts, VideoOptions{}),
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

func TestCompileVideoV1MatchesOfficialMatrix(t *testing.T) {
	tenSeconds := int64(10_000)
	optimizer := true
	pretreatment := false

	with := func(model string, parts []message.Part, mutate func(*inference.VideoIntent)) videoWire {
		t.Helper()
		request := compileVideoRequest(parts, VideoOptions{
			PromptOptimizer:  &optimizer,
			FastPretreatment: &pretreatment,
			CallbackURL:      "https://example.com/callback",
		})
		if mutate != nil {
			mutate(request.Input.Content.Intent.Video)
		}
		wire, report, err := compileVideoWire(t, model, request)
		if err != nil {
			t.Fatalf("compile %s: %v; report = %+v", model, err, report)
		}
		return wire
	}

	t.Run("fast accepts 10s at 768P and 1080P at 6s", func(t *testing.T) {
		parts := []message.Part{
			message.TextPart{Text: "a scene"},
			videoImagePart(t, "https://example.com/a.png"),
		}
		wire := with("MiniMax-Hailuo-2.3-Fast", parts, func(i *inference.VideoIntent) {
			i.DurationMillis = &tenSeconds
			i.Resolution = "768P"
		})
		if wire.duration == nil || *wire.duration != 10 || wire.resolution != "768P" {
			t.Fatalf("wire = %+v", wire)
		}
		if wire.promptOptimizer == nil || !*wire.promptOptimizer {
			t.Fatal("prompt_optimizer not set")
		}
		if wire.fastPretreatment == nil || *wire.fastPretreatment {
			t.Fatal("fast_pretreatment not set to false")
		}
		if wire.callbackURL != "https://example.com/callback" {
			t.Fatalf("callbackURL = %q", wire.callbackURL)
		}
	})

	t.Run("hailuo 02 accepts 512P for single first-frame i2v", func(t *testing.T) {
		wire := with("MiniMax-Hailuo-02", []message.Part{
			message.TextPart{Text: "a scene"},
			videoImagePart(t, "https://example.com/a.png"),
		}, func(i *inference.VideoIntent) {
			i.DurationMillis = &tenSeconds
			i.Resolution = "512P"
		})
		if wire.firstFrame != "https://example.com/a.png" || wire.lastFrame != "" ||
			wire.resolution != "512P" || wire.duration == nil || *wire.duration != 10 {
			t.Fatalf("wire = %+v", wire)
		}
	})
}

func TestCompileVideoV1RejectsOutOfMatrix(t *testing.T) {
	tenSeconds := int64(10_000)
	image := videoImagePart(t, "https://example.com/a.png")

	cases := []struct {
		name   string
		model  string
		parts  []message.Part
		intent func(*inference.VideoIntent)
		field  inference.FieldID
		reason string
	}{
		{
			name:   "fast without first frame",
			model:  "MiniMax-Hailuo-2.3-Fast",
			parts:  []message.Part{message.TextPart{Text: "a scene"}},
			field:  inference.FieldGenerateIntentVideo,
			reason: "image-to-video only",
		},
		{
			name:  "fast 10s at 1080P",
			model: "MiniMax-Hailuo-2.3-Fast",
			parts: []message.Part{message.TextPart{Text: "a scene"}, image},
			intent: func(i *inference.VideoIntent) {
				i.DurationMillis = &tenSeconds
				i.Resolution = "1080P"
			},
			field:  inference.FieldGenerateIntentVideoDuration,
			reason: "10-second videos require 768P",
		},
		{
			name:  "hailuo 02 512P on text-to-video",
			model: "MiniMax-Hailuo-02",
			parts: []message.Part{message.TextPart{Text: "a scene"}},
			intent: func(i *inference.VideoIntent) {
				i.Resolution = "512P"
			},
			field:  inference.FieldGenerateIntentVideoResolution,
			reason: "768P/1080P tiers",
		},
		{
			name:  "hailuo 02 512P with first and last frame",
			model: "MiniMax-Hailuo-02",
			parts: []message.Part{
				message.TextPart{Text: "a scene"},
				videoImagePart(t, "https://example.com/a.png"),
				videoImagePart(t, "https://example.com/b.png"),
			},
			intent: func(i *inference.VideoIntent) {
				i.Resolution = "512P"
			},
			field:  inference.FieldGenerateIntentVideoResolution,
			reason: "768P/1080P tiers",
		},
		{
			name:  "hailuo 02 rejects three images",
			model: "MiniMax-Hailuo-02",
			parts: []message.Part{
				message.TextPart{Text: "a scene"},
				videoImagePart(t, "https://example.com/a.png"),
				videoImagePart(t, "https://example.com/b.png"),
				videoImagePart(t, "https://example.com/c.png"),
			},
			field:  inference.FieldGenerateInputImage,
			reason: "first-frame and a last-frame image",
		},
		{
			name:  "hailuo 2.3 rejects two images",
			model: "MiniMax-Hailuo-2.3",
			parts: []message.Part{
				message.TextPart{Text: "a scene"},
				videoImagePart(t, "https://example.com/a.png"),
				videoImagePart(t, "https://example.com/b.png"),
			},
			field:  inference.FieldGenerateInputImage,
			reason: "single first-frame image",
		},
		{
			name:  "hailuo 2.3 rejects video reference",
			model: "MiniMax-Hailuo-2.3",
			parts: []message.Part{
				message.TextPart{Text: "a scene"},
				videoClipPart(t, "https://example.com/clip.mp4"),
			},
			field:  inference.FieldGenerateInputVideo,
			reason: "does not accept reference-video",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := compileVideoRequest(tc.parts, VideoOptions{})
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

func TestCompileVideoPromptLengthLimits(t *testing.T) {
	reject := func(
		t *testing.T,
		model string,
		parts []message.Part,
		field inference.FieldID,
		reason string,
	) {
		t.Helper()
		_, report, err := compileVideoWire(
			t,
			model,
			compileVideoRequest(parts, VideoOptions{}),
		)
		if err == nil {
			t.Fatalf("compile unexpectedly succeeded; report = %+v", report)
		}
		got := rejectedReason(report, field)
		if !strings.Contains(got, reason) {
			t.Errorf("rejection reason = %q, want substring %q", got, reason)
		}
	}

	t.Run("v1 caps at 2000 characters", func(t *testing.T) {
		reject(
			t,
			"MiniMax-Hailuo-2.3",
			[]message.Part{message.TextPart{Text: strings.Repeat("a", 2001)}},
			inference.FieldGenerateInputText,
			"at most 2000 characters",
		)
		request := compileVideoRequest(
			[]message.Part{message.TextPart{Text: strings.Repeat("a", 2000)}},
			VideoOptions{},
		)
		if _, report, err := compileVideoWire(t, "MiniMax-Hailuo-2.3", request); err != nil {
			t.Fatalf("2000-char prompt rejected: %v; report = %+v", err, report)
		}
	})

	t.Run("v2 caps at 7000 characters", func(t *testing.T) {
		reject(
			t,
			"MiniMax-H3",
			[]message.Part{message.TextPart{Text: strings.Repeat("a", 7001)}},
			inference.FieldGenerateInputText,
			"at most 7000 characters",
		)
		request := compileVideoRequest(
			[]message.Part{message.TextPart{Text: strings.Repeat("a", 7000)}},
			VideoOptions{},
		)
		if _, report, err := compileVideoWire(t, "MiniMax-H3", request); err != nil {
			t.Fatalf("7000-char prompt rejected: %v; report = %+v", err, report)
		}
	})

	t.Run("oversized context text rejects on context field", func(t *testing.T) {
		request := compileVideoRequest(
			[]message.Part{videoImagePart(t, "https://example.com/a.png")},
			VideoOptions{},
		)
		request.Context = []message.Message{{
			Role: message.RoleUser,
			Content: message.Content{
				Parts: []message.Part{message.TextPart{Text: strings.Repeat("b", 2001)}},
			},
		}}
		_, report, err := compileVideoWire(t, "MiniMax-Hailuo-2.3", request)
		if err == nil {
			t.Fatalf("compile unexpectedly succeeded; report = %+v", report)
		}
		got := rejectedReason(report, inference.FieldGenerateContextText)
		if !strings.Contains(got, "at most 2000 characters") {
			t.Errorf("rejection reason = %q", got)
		}
	})
}

func TestCompileVideoLastFrameOnly(t *testing.T) {
	lastOnly := true
	image := videoImagePart(t, "https://example.com/a.png")

	t.Run("v2 single image becomes the last frame", func(t *testing.T) {
		request := compileVideoRequest(
			[]message.Part{message.TextPart{Text: "a scene"}, image},
			VideoOptions{LastFrameOnly: &lastOnly},
		)
		wire, report, err := compileVideoWire(t, "MiniMax-H3", request)
		if err != nil {
			t.Fatalf("compile: %v; report = %+v", err, report)
		}
		if wire.firstFrame != "" || wire.lastFrame != "https://example.com/a.png" {
			t.Fatalf("frames = %q / %q, want last-frame-only", wire.firstFrame, wire.lastFrame)
		}
	})

	t.Run("v2 requires exactly one image", func(t *testing.T) {
		request := compileVideoRequest(
			[]message.Part{
				message.TextPart{Text: "a scene"},
				videoImagePart(t, "https://example.com/a.png"),
				videoImagePart(t, "https://example.com/b.png"),
			},
			VideoOptions{LastFrameOnly: &lastOnly},
		)
		_, report, err := compileVideoWire(t, "MiniMax-H3", request)
		if err == nil {
			t.Fatalf("compile unexpectedly succeeded; report = %+v", report)
		}
		reason := rejectedReason(report, videoOptionsField("last_frame_only"))
		if !strings.Contains(reason, "exactly one input image") {
			t.Errorf("rejection reason = %q", reason)
		}
	})

	t.Run("v1 rejects last-frame-only tasks", func(t *testing.T) {
		request := compileVideoRequest(
			[]message.Part{message.TextPart{Text: "a scene"}, image},
			VideoOptions{LastFrameOnly: &lastOnly},
		)
		_, report, err := compileVideoWire(t, "MiniMax-Hailuo-02", request)
		if err == nil {
			t.Fatalf("compile unexpectedly succeeded; report = %+v", report)
		}
		reason := rejectedReason(report, videoOptionsField("last_frame_only"))
		if !strings.Contains(reason, "no last-frame-only task") {
			t.Errorf("rejection reason = %q", reason)
		}
	})
}

func TestVideoImageValueDataURI(t *testing.T) {
	payload := []byte{0x89, 'P', 'N', 'G'}
	inline := videoInlineImagePart(t, payload)
	got := videoImageValue(inline.Source)
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(payload)
	if got != want {
		t.Fatalf("videoImageValue = %q, want %q", got, want)
	}

	remote := videoImagePart(t, "https://example.com/a.png")
	if got := videoImageValue(remote.Source); got != "https://example.com/a.png" {
		t.Fatalf("videoImageValue(URL) = %q", got)
	}
}

func TestVideoTaskStreamSourceRejects(t *testing.T) {
	pipe := media.NewPipe[string](1)
	source, err := media.NewVideoStream(pipe, "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoStream: %v", err)
	}
	_, report, err := compileVideoWire(t, "MiniMax-H3", compileVideoRequest(
		[]message.Part{message.VideoPart{Source: source}},
		VideoOptions{},
	))
	if err == nil {
		t.Fatal("video task accepted a stream video source")
	}
	if !report.Rejects(inference.FieldGenerateInputVideo) {
		t.Fatal("video task did not reject the stream on the input video field")
	}
}

func TestTransportVideoV2(t *testing.T) {
	var polls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v2/video_generation":
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			if body["model"] != "MiniMax-H3" ||
				body["resolution"] != "768P" ||
				body["duration"] != float64(6) ||
				body["ratio"] != "16:9" ||
				body["aigc_watermark"] != true ||
				body["callback_url"] != "https://example.com/callback" {
				t.Errorf("create body = %#v", body)
			}
			content, ok := body["content"].([]any)
			if !ok || len(content) != 2 {
				t.Fatalf("content = %#v", body["content"])
			}
			text := content[0].(map[string]any)
			image := content[1].(map[string]any)
			if text["type"] != "text" || text["text"] != "a scene" ||
				image["type"] != "image_url" || image["role"] != "first_frame" {
				t.Errorf("content items = %#v / %#v", text, image)
			}
			_, _ = io.WriteString(writer, `{"task_id":"t-v2"}`)
		case request.Method == http.MethodGet &&
			request.URL.Path == "/v2/query/video_generation/t-v2":
			polls++
			if polls < 2 {
				_, _ = io.WriteString(writer, `{"task":{"id":"t-v2","status":"running"}}`)
			} else {
				_, _ = io.WriteString(writer,
					`{"task":{"id":"t-v2","status":"succeeded","content":{"url":"https://cdn.example/v2.mp4"}}}`)
			}
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()

	client := newMediaClient("sk-test", server.URL, Spec{})
	watermark := true
	raw, err := transportVideoV2(client, time.Millisecond)(context.Background(), videoWire{
		model:  "MiniMax-H3",
		prompt: "a scene",
		v2Content: v2Content{
			firstFrame: "https://example.com/a.png",
		},
		duration:    intPointer(6),
		resolution:  "768P",
		ratio:       "16:9",
		watermark:   &watermark,
		callbackURL: "https://example.com/callback",
	})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if raw.videoURL != "https://cdn.example/v2.mp4" || raw.requestID != "t-v2" {
		t.Fatalf("raw = %+v", raw)
	}
	if polls != 2 {
		t.Fatalf("polls = %d, want 2", polls)
	}
}

func TestTransportVideoV2TerminalErrors(t *testing.T) {
	cases := []struct {
		name    string
		status  string
		wantErr string
	}{
		{name: "failed", status: "failed", wantErr: "failed server-side"},
		{name: "cancelled", status: "cancelled", wantErr: "cancelled server-side"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				switch request.Method {
				case http.MethodPost:
					_, _ = io.WriteString(writer, `{"task_id":"t-v2"}`)
				case http.MethodGet:
					_, _ = io.WriteString(writer, `{"task":{"id":"t-v2","status":"`+tc.status+`"}}`)
				}
			}))
			defer server.Close()

			client := newMediaClient("sk-test", server.URL, Spec{})
			_, err := transportVideoV2(client, time.Millisecond)(context.Background(), videoWire{
				model:      "MiniMax-H3",
				prompt:     "a scene",
				duration:   intPointer(6),
				resolution: "768P",
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestTransportVideoV1WireBody(t *testing.T) {
	inline := videoInlineImagePart(t, []byte{0x89, 'P', 'N', 'G'})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/video_generation":
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			if body["model"] != "MiniMax-Hailuo-02" ||
				body["duration"] != float64(10) ||
				body["resolution"] != "512P" ||
				body["aigc_watermark"] != false ||
				body["prompt_optimizer"] != true ||
				body["fast_pretreatment"] != false ||
				body["callback_url"] != "https://example.com/callback" {
				t.Errorf("create body = %#v", body)
			}
			firstFrame, _ := body["first_frame_image"].(string)
			if !strings.HasPrefix(firstFrame, "data:image/png;base64,") {
				t.Errorf("first_frame_image = %q, want data URI", firstFrame)
			}
			if body["last_frame_image"] != "https://example.com/b.png" {
				t.Errorf("last_frame_image = %#v", body["last_frame_image"])
			}
			_, _ = io.WriteString(writer,
				`{"task_id":"t1","base_resp":{"status_code":0,"status_msg":"success"}}`)
		case request.Method == http.MethodGet &&
			request.URL.Path == "/v1/query/video_generation" &&
			request.URL.Query().Get("task_id") == "t1":
			_, _ = io.WriteString(writer,
				`{"task_id":"t1","status":"Success","file_id":"f1","base_resp":{"status_code":0,"status_msg":"success"}}`)
		case request.Method == http.MethodGet &&
			request.URL.Path == "/v1/files/retrieve" &&
			request.URL.Query().Get("file_id") == "f1":
			_, _ = io.WriteString(writer,
				`{"file":{"download_url":"https://cdn.example/v1.mp4"},"base_resp":{"status_code":0,"status_msg":"success"}}`)
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()

	client := newMediaClient("sk-test", server.URL, Spec{})
	watermark := false
	optimizer := true
	pretreatment := false
	raw, err := transportVideoV1(client, time.Millisecond)(context.Background(), videoWire{
		model:  "MiniMax-Hailuo-02",
		prompt: "a scene",
		v2Content: v2Content{
			firstFrame: videoImageValue(inline.Source),
			lastFrame:  "https://example.com/b.png",
		},
		duration:         intPointer(10),
		resolution:       "512P",
		watermark:        &watermark,
		promptOptimizer:  &optimizer,
		fastPretreatment: &pretreatment,
		callbackURL:      "https://example.com/callback",
	})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if raw.videoURL != "https://cdn.example/v1.mp4" || raw.requestID != "t1" {
		t.Fatalf("raw = %+v", raw)
	}
}

func intPointer(value int) *int {
	return &value
}
