package bytedance

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/inference/pipeline"
)

// VideoOptions declarations the video surface refuses are gated on what the model
// claims, so a table case says which declaration it runs against: a bare model
// supports nothing beyond text, and the full one supports everything.
type videoDeclaration int

const (
	declaredNothing videoDeclaration = iota
	declaredEverything
)

// videoOptionsSpec is a model that supports everything, so a rejection can be
// attributed to the option rather than to a missing declaration.
func videoOptionsSpec() ModelSpec {
	min, max := int64(4), int64(12)
	return ModelSpec{
		Name: "video-options", Kind: "video",
		Capabilities: model.ModelCapabilities{
			Inputs:          []message.PartKind{message.PartText, message.PartImage},
			Modes:           []model.Mode{model.ModeVideo},
			HostedWebSearch: true,
		},
		MaxResolution: "1080p",
		Video: VideoParams{
			Seed: true, CameraFixed: true, FlexTier: true, GenerateAudio: true,
			Priority: true, OutputFormat: true, OmniReference: true,
			DurationMin: &min, DurationMax: &max,
			ReferenceImage: 4,
		},
	}
}

// bareVideoSpec supports the canonical text-in, video-out shape and nothing else.
func bareVideoSpec() ModelSpec {
	return ModelSpec{
		Name: "video-bare", Kind: "video",
		Capabilities: model.ModelCapabilities{
			Inputs: []message.PartKind{message.PartText},
			Modes:  []model.Mode{model.ModeVideo},
		},
		Video: VideoParams{DurationMin: func() *int64 { v := int64(4); return &v }(),
			DurationMax: func() *int64 { v := int64(12); return &v }()},
		MaxResolution: "1080p",
	}
}

func (d videoDeclaration) spec() ModelSpec {
	if d == declaredEverything {
		return videoOptionsSpec()
	}
	return bareVideoSpec()
}

func compileTestVideo(t *testing.T, spec ModelSpec, request inference.GenerateRequest) (*videoRequest, error) {
	t.Helper()
	compiled, err := compileVideo("ep-video", spec)(
		context.Background(), request, pipeline.ShapeUnary)
	return compiled.Wire, err
}

func videoOptionsRequest(t *testing.T, options *VideoOptions, parts ...message.Part) inference.GenerateRequest {
	t.Helper()
	request := testRequest("video-options", inference.OutputVideo, message.Content{Parts: parts})
	request.Extensions = inference.Extensions{options}
	return request
}

// The video surface carries the most options and the most declarations gating
// them, so the mapping test is the one that would catch a field wired to the
// wrong wire name.
func TestCompileVideoAppliesOptions(t *testing.T) {
	wire, err := compileTestVideo(t, videoOptionsSpec(), videoOptionsRequest(t, &VideoOptions{
		DurationMillis:        extPtr(int64(6000)),
		Resolution:            "720P",
		AspectRatio:           "16:9",
		Seed:                  extPtr(int64(11)),
		Watermark:             extPtr(true),
		CameraFixed:           extPtr(false),
		GenerateAudio:         extPtr(true),
		ServiceTier:           "flex",
		Priority:              extPtr(int32(7)),
		OutputFormat:          extPtr("mov"),
		OmniReferenceTaskType: extPtr("reference"),
		WebSearch:             extPtr(true),
		ExecutionExpiresAfter: extPtr(int64(3600)),
		CallbackURL:           extPtr("https://example.test/hook"),
		SafetyIdentifier:      extPtr("tenant-7"),
	}, message.TextPart{Text: "a calm lake"}))
	if err != nil {
		t.Fatalf("compile: %s", causeText(err))
	}
	if wire.Duration == nil || *wire.Duration != 6 {
		t.Errorf("duration = %v, want 6s", wire.Duration)
	}
	if wire.Resolution == nil || *wire.Resolution != "720p" {
		t.Errorf("resolution = %v, want the lower-cased 720p", wire.Resolution)
	}
	if wire.Ratio == nil || *wire.Ratio != "16:9" {
		t.Errorf("ratio = %v", wire.Ratio)
	}
	if wire.Seed == nil || *wire.Seed != 11 {
		t.Errorf("seed = %v", wire.Seed)
	}
	if wire.Watermark == nil || !*wire.Watermark {
		t.Errorf("watermark = %v, want true", wire.Watermark)
	}
	if wire.CameraFixed == nil || *wire.CameraFixed {
		t.Errorf("camera_fixed = %v, want false", wire.CameraFixed)
	}
	if wire.GenerateAudio == nil || !*wire.GenerateAudio {
		t.Errorf("generate_audio = %v, want true", wire.GenerateAudio)
	}
	if wire.ServiceTier == nil || *wire.ServiceTier != "flex" {
		t.Errorf("service_tier = %v", wire.ServiceTier)
	}
	if wire.Priority == nil || *wire.Priority != 7 {
		t.Errorf("priority = %v", wire.Priority)
	}
	if wire.OutputFormat == nil || *wire.OutputFormat != "mov" {
		t.Errorf("output_format = %v", wire.OutputFormat)
	}
	if wire.OmniReferenceTaskType == nil || *wire.OmniReferenceTaskType != "reference" {
		t.Errorf("omni = %v", wire.OmniReferenceTaskType)
	}
	if len(wire.Tools) != 1 || wire.Tools[0].Type != "web_search" {
		t.Errorf("tools = %+v", wire.Tools)
	}
	if wire.ExecutionExpiresAfter == nil || *wire.ExecutionExpiresAfter != 3600 {
		t.Errorf("expires = %v", wire.ExecutionExpiresAfter)
	}
	if wire.CallbackURL == nil || *wire.CallbackURL != "https://example.test/hook" {
		t.Errorf("callback = %v", wire.CallbackURL)
	}
	if wire.SafetyIdentifier == nil || *wire.SafetyIdentifier != "tenant-7" {
		t.Errorf("safety_identifier = %v", wire.SafetyIdentifier)
	}
}

// TestCompileVideoRefusesUnsupportedOptions is the declaration side: a model that
// does not claim a capability must not accept its option — and one that does must
// accept it, which is why the same option appears under both declarations. Each of
// these is a deployment mistake that would otherwise reach the platform and come
// back as an opaque 400.
func TestCompileVideoRefusesUnsupportedOptions(t *testing.T) {
	cases := []struct {
		name        string
		options     *VideoOptions
		declaration videoDeclaration
		want        string
	}{
		{"seed", &VideoOptions{Seed: extPtr(int64(1))}, declaredNothing, "does not support seed"},
		{"camera", &VideoOptions{CameraFixed: extPtr(true)}, declaredNothing,
			"does not support camera_fixed"},
		{"audio", &VideoOptions{GenerateAudio: extPtr(true)}, declaredNothing,
			"does not support generate_audio"},
		{"flex tier", &VideoOptions{ServiceTier: "flex"}, declaredNothing,
			"does not support flex tier"},
		{"priority", &VideoOptions{Priority: extPtr(int32(1))}, declaredNothing,
			"does not support priority"},
		{"format", &VideoOptions{OutputFormat: extPtr("mov")}, declaredNothing,
			"does not support output_format"},
		{"omni", &VideoOptions{OmniReferenceTaskType: extPtr("reference")}, declaredNothing,
			"does not support omni_reference_task_type"},
		{"web search", &VideoOptions{WebSearch: extPtr(true)}, declaredNothing,
			"does not support web_search"},
		{"whole seconds", &VideoOptions{DurationMillis: extPtr(int64(6500))}, declaredNothing,
			"duration must be whole seconds"},
		{"duration floor", &VideoOptions{DurationMillis: extPtr(int64(2000))}, declaredNothing,
			"at least 4s"},
		{"duration ceiling", &VideoOptions{DurationMillis: extPtr(int64(20000))}, declaredNothing,
			"at most 12s"},
		{"resolution cap", &VideoOptions{Resolution: "4K"}, declaredNothing,
			"resolution is capped at 1080p"},
		{"unknown resolution", &VideoOptions{Resolution: "huge"}, declaredNothing,
			"resolution is capped at 1080p"},
		{"unknown ratio", &VideoOptions{AspectRatio: "5:4"}, declaredNothing,
			`unsupported ratio "5:4"`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			spec := test.declaration.spec()
			_, err := compileTestVideo(t, spec, videoOptionsRequest(t, test.options,
				message.TextPart{Text: "x"}))
			if err == nil {
				t.Fatalf("%s was accepted", test.name)
			}
			if got := causeText(err); !strings.Contains(got, test.want) {
				t.Fatalf("error = %q, want it to name %q", got, test.want)
			}
		})
	}

	// The same declarations, on a model that claims them: each has to compile.
	accepted := []*VideoOptions{
		{Seed: extPtr(int64(1))},
		{CameraFixed: extPtr(true)},
		{GenerateAudio: extPtr(false)},
		{ServiceTier: "flex"},
		{Priority: extPtr(int32(3))},
		{OutputFormat: extPtr("mov")},
		{OmniReferenceTaskType: extPtr("extend")},
		{WebSearch: extPtr(true)},
		{DurationMillis: extPtr(int64(6000))},
	}
	for _, options := range accepted {
		if _, err := compileTestVideo(t, videoOptionsSpec(), videoOptionsRequest(t,
			options, message.TextPart{Text: "x"})); err != nil {
			t.Errorf("a declaration the model claims was refused: %s", causeText(err))
		}
	}
}

// A first/last-frame task pins the ratio: the platform derives it from the frames,
// so asking for another one is a contradiction.
func TestCompileVideoRequiresAdaptiveRatioForFrames(t *testing.T) {
	spec := videoOptionsSpec()
	spec.Video.FrameRatioAdaptiveOnly = true
	image := embedTestImagePart(t)
	if _, err := compileTestVideo(t, spec, videoOptionsRequest(t,
		&VideoOptions{AspectRatio: "16:9"}, message.TextPart{Text: "x"}, image)); err == nil {
		t.Fatal("a fixed ratio was accepted for a first-frame task")
	} else if got := causeText(err); !strings.Contains(got, "require ratio=adaptive") {
		t.Fatalf("error = %q", got)
	}
	if _, err := compileTestVideo(t, spec, videoOptionsRequest(t,
		&VideoOptions{AspectRatio: "adaptive"}, message.TextPart{Text: "x"}, image)); err != nil {
		t.Fatalf("adaptive ratio refused: %s", causeText(err))
	}
}

// The content items are what the platform reads media by, so the role and the
// media key both have to be right: a first frame sent as a reference is a
// different task.
func TestVideoContentItems(t *testing.T) {
	image := itemImage("https://example.test/frame.png", "first_frame")
	if image.Type != videoContentImage || image.ImageURL == nil ||
		image.ImageURL.URL != "https://example.test/frame.png" ||
		image.Role == nil || *image.Role != "first_frame" {
		t.Fatalf("image item = %+v", image)
	}
	video := itemVideo("https://example.test/clip.mp4", "reference_video")
	if video.Type != videoContentVideo || video.VideoURL == nil ||
		video.VideoURL.URL != "https://example.test/clip.mp4" {
		t.Fatalf("video item = %+v", video)
	}
	audio := itemAudio("https://example.test/voice.mp3", "reference_audio")
	if audio.Type != videoContentAudio || audio.AudioURL == nil ||
		audio.AudioURL.URL != "https://example.test/voice.mp3" {
		t.Fatalf("audio item = %+v", audio)
	}
	request := &videoRequest{Content: []*videoContent{image, video, audio}}
	if !hasContentRole(request, "first_frame") {
		t.Error("first_frame not found among the items")
	}
	if hasContentRole(request, "last_frame") {
		t.Error("last_frame reported for a request that has none")
	}
}

// The two vocabularies the platform validates by hand: ratios it accepts, and the
// resolution tiers it compares by size.
func TestVideoVocabulary(t *testing.T) {
	for _, ratio := range []string{"16:9", "4:3", "1:1", "3:4", "9:16", "21:9", "adaptive"} {
		if !validVideoRatio(ratio) {
			t.Errorf("%s was refused", ratio)
		}
	}
	for _, ratio := range []string{"5:4", "16:9 ", "", "auto"} {
		if validVideoRatio(ratio) {
			t.Errorf("%q was accepted", ratio)
		}
	}
	cases := []struct {
		resolution, cap string
		want            bool
	}{
		{"720p", "1080p", true},
		{"1080p", "1080p", true},
		// 2K is 1080 lines: the tiers are compared by line count, not by name.
		{"2k", "1080p", true},
		{"4k", "1080p", false},
		{"480p", "1k", true},
	}
	for _, test := range cases {
		if got := resolutionWithin(test.resolution, test.cap); got != test.want {
			t.Errorf("resolutionWithin(%q, %q) = %v, want %v",
				test.resolution, test.cap, got, test.want)
		}
	}
	// An unparseable tier on either side is not "within" anything: the caller
	// reads the false as a refused request rather than as a silent downgrade.
	for _, tier := range []string{"huge", ""} {
		if resolutionWithin(tier, "1080p") || resolutionWithin("720p", tier) {
			t.Errorf("tier %q compared as a resolution", tier)
		}
	}
}
