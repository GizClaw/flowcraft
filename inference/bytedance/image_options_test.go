package bytedance

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/inference/pipeline"
)

// imageOptionsRequest builds an image request carrying the platform's own
// options, which is how a deployment's extension block reaches the compiler.
func imageOptionsRequest(t *testing.T, options *ImageOptions, parts ...message.Part) inference.GenerateRequest {
	t.Helper()
	request := testRequest("image-options", inference.OutputImage, message.Content{Parts: parts})
	request.Extensions = inference.Extensions{options}
	return request
}

func compileTestImage(
	t *testing.T,
	descriptor model.ModelDescriptor,
	request inference.GenerateRequest,
) (*imageRequest, []inference.Warning, error) {
	t.Helper()
	compiled, err := compileImage("ep-image", descriptor)(
		context.Background(), request, pipeline.ShapeUnary)
	return compiled.Wire, compiled.Warnings, err
}

// causeText is the sentence behind an inference error. [inference.Error.Error]
// is the redacted form a model sees, so an assertion about why a declaration was
// refused reads the cause instead.
func causeText(err error) string {
	var inferenceErr *inference.Error
	if errors.As(err, &inferenceErr) {
		if cause := inferenceErr.Unwrap(); cause != nil {
			return cause.Error()
		}
	}
	return err.Error()
}

// The image options are a mapping from a deployment's declaration to the wire, so
// what is asserted is the wire: every field the platform sets has to survive the
// trip, and the ones it does not set must stay absent.
func TestCompileImageAppliesOptions(t *testing.T) {
	descriptor := testDescriptor("image-options", "image",
		[]message.PartKind{message.PartText}, []message.PartKind{message.PartImage})
	five := 5
	guidance := 7.5
	watermark := false
	wire, _, err := compileTestImage(t, descriptor, imageOptionsRequest(t, &ImageOptions{
		Size:                &media.ImageSize{Width: 2048, Height: 2048},
		Seed:                extPtr(int64(42)),
		OutputFormat:        media.ImageFormatJPEG,
		Delivery:            media.SourceInline,
		GuidanceScale:       &guidance,
		Watermark:           &watermark,
		OptimizePrompt:      &ImageOptimizePrompt{Mode: "standard", Thinking: "enabled"},
		Sequential:          extPtr(true),
		SequentialMaxImages: &five,
		WebSearch:           extPtr(true),
	}, message.TextPart{Text: "a red square"}))
	if err != nil {
		t.Fatalf("compile: %s", causeText(err))
	}
	if wire.ark.Size == nil || *wire.ark.Size != "2048x2048" {
		t.Errorf("size = %v, want 2048x2048", wire.ark.Size)
	}
	if wire.count != 1 {
		t.Errorf("count = %d, want 1", wire.count)
	}
	if wire.ark.Seed == nil || *wire.ark.Seed != 42 {
		t.Errorf("seed = %v, want 42", wire.ark.Seed)
	}
	if wire.ark.OutputFormat == nil || *wire.ark.OutputFormat != "jpeg" {
		t.Errorf("format = %v, want jpeg", wire.ark.OutputFormat)
	}
	if wire.ark.ResponseFormat == nil || *wire.ark.ResponseFormat != "b64_json" {
		t.Errorf("delivery = %v, want b64_json for inline", wire.ark.ResponseFormat)
	}
	if wire.ark.GuidanceScale == nil || *wire.ark.GuidanceScale != 7.5 {
		t.Errorf("guidance = %v", wire.ark.GuidanceScale)
	}
	if wire.ark.Watermark == nil || *wire.ark.Watermark {
		t.Errorf("watermark = %v, want false", wire.ark.Watermark)
	}
	if wire.ark.OptimizePrompt == nil || !*wire.ark.OptimizePrompt {
		t.Error("optimize_prompt was not enabled")
	}
	if opt := wire.ark.OptimizePromptOptions; opt == nil ||
		opt.Mode != "standard" || opt.Thinking != "enabled" {
		t.Errorf("optimize options = %+v", opt)
	}
	if wire.ark.SequentialImageGeneration == nil || *wire.ark.SequentialImageGeneration != imageSequentialAuto {
		t.Errorf("sequential = %v, want %q", wire.ark.SequentialImageGeneration, imageSequentialAuto)
	}
	if opt := wire.ark.SequentialImageGenerationOptions; opt == nil ||
		opt.MaxImages == nil || *opt.MaxImages != 5 {
		t.Errorf("sequential options = %+v, want max 5", opt)
	}
	if len(wire.ark.Tools) != 1 || wire.ark.Tools[0].Type != "web_search" {
		t.Errorf("tools = %+v, want web_search", wire.ark.Tools)
	}
}

// A quality the platform has no parameter for is a warning, not a failure: a
// deployment that declares one should be told it was read and dropped.
func TestCompileImageWarnsAboutQuality(t *testing.T) {
	descriptor := testDescriptor("image-options", "image",
		[]message.PartKind{message.PartText}, []message.PartKind{message.PartImage})
	_, warnings, err := compileTestImage(t, descriptor, imageOptionsRequest(t, &ImageOptions{
		Quality: media.ImageQualityHigh,
	}, message.TextPart{Text: "x"}))
	if err != nil {
		t.Fatalf("compile: %s", causeText(err))
	}
	if len(warnings) != 1 || warnings[0].Code != "quality_ignored" {
		t.Fatalf("warnings = %+v, want one quality_ignored", warnings)
	}
}

// Every conflict the image surface refuses: they are the declarations a
// deployment gets wrong, and each has a sentence worth asserting.
func TestCompileImageRefusesConflictingOptions(t *testing.T) {
	descriptor := testDescriptor("image-options", "image",
		[]message.PartKind{message.PartText, message.PartImage}, []message.PartKind{message.PartImage})
	refOne := embedTestImagePart(t)
	refTwo := embedTestImagePart(t)
	size := &media.ImageSize{Width: 1024, Height: 1024}
	cases := []struct {
		name    string
		options *ImageOptions
		parts   []message.Part
		want    string
	}{
		{
			name:    "size token with size",
			options: &ImageOptions{SizeToken: "2k", Size: size},
			parts:   []message.Part{message.TextPart{Text: "x"}},
			want:    "size_token conflicts with size",
		},
		{
			name:    "sequential count twice",
			options: &ImageOptions{Sequential: extPtr(true), SequentialMaxImages: extPtr(4), Count: extPtr(2)},
			parts:   []message.Part{message.TextPart{Text: "x"}},
			want:    "sequential_max_images conflicts with count",
		},
		{
			name:    "seed on a batch",
			options: &ImageOptions{Seed: extPtr(int64(1)), Count: extPtr(2)},
			parts:   []message.Part{message.TextPart{Text: "x"}},
			want:    "seed is supported for single-image requests only",
		},
		{
			name:    "layers without a reference",
			options: &ImageOptions{LayerDecomposition: extPtr(true)},
			parts:   []message.Part{message.TextPart{Text: "x"}},
			want:    "layer_decomposition requires an input image",
		},
		{
			name:    "layers with a size",
			options: &ImageOptions{LayerDecomposition: extPtr(true), Size: size},
			parts:   []message.Part{refOne, message.TextPart{Text: "x"}},
			want:    "layer_decomposition supports size_token only",
		},
		{
			name:    "layers with sequential",
			options: &ImageOptions{LayerDecomposition: extPtr(true), Sequential: extPtr(true)},
			parts:   []message.Part{refOne, message.TextPart{Text: "x"}},
			want:    "layer_decomposition conflicts with sequential generation",
		},
		{
			name:    "background without a reference",
			options: &ImageOptions{Background: "transparent"},
			parts:   []message.Part{message.TextPart{Text: "x"}},
			want:    "background requires exactly one input image",
		},
		{
			name:    "background with two references",
			options: &ImageOptions{Background: "transparent"},
			parts:   []message.Part{refOne, refTwo, message.TextPart{Text: "x"}},
			want:    "background requires exactly one input image",
		},
		{
			name:    "background with sequential",
			options: &ImageOptions{Background: "transparent", Sequential: extPtr(true)},
			parts:   []message.Part{refOne, message.TextPart{Text: "x"}},
			want:    "background conflicts with sequential generation",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := compileTestImage(t, descriptor,
				imageOptionsRequest(t, test.options, test.parts...))
			if err == nil {
				t.Fatalf("%s was accepted", test.name)
			}
			if got := causeText(err); !strings.Contains(got, test.want) {
				t.Fatalf("error = %q, want it to name %q", got, test.want)
			}
		})
	}
}

// A size token is the platform's own vocabulary: the tier names are upper-cased
// except for the one the platform reads as a word.
func TestCompileImageNormalisesSizeTokens(t *testing.T) {
	descriptor := testDescriptor("image-options", "image",
		[]message.PartKind{message.PartText}, []message.PartKind{message.PartImage})
	for token, want := range map[string]string{
		"2k":       "2K",
		"4k":       "4K",
		"adaptive": "adaptive",
	} {
		wire, _, err := compileTestImage(t, descriptor, imageOptionsRequest(t,
			&ImageOptions{SizeToken: token}, message.TextPart{Text: "x"}))
		if err != nil {
			t.Fatalf("%s: %s", token, causeText(err))
		}
		if wire.ark.Size == nil || *wire.ark.Size != want {
			t.Errorf("%s: size = %v, want %s", token, wire.ark.Size, want)
		}
	}
}
