package minimax

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// imageRequestWithOptions builds one image generation request carrying the
// intent and the ImageOptions extension under test.
func imageRequestWithOptions(
	intent inference.ImageIntent,
	options ImageOptions,
) inference.GenerateRequest {
	var extensions inference.Extensions
	extensions = append(extensions, options)
	return inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "a red circle"},
				}},
				Intent: inference.Intent{Image: &intent},
			},
		},
		Extensions: extensions,
	}
}

func compileImageWire(
	t *testing.T,
	request inference.GenerateRequest,
) (imageWire, inference.CompileReport, error) {
	t.Helper()
	compiled, err := compileImage("ep-test")(
		context.Background(),
		model.ModelRef{
			ID: model.ModelID{Provider: providerID, Name: "image-01"},
		},
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		return imageWire{}, compiled.Report, err
	}
	return compiled.Wire, compiled.Report, nil
}

func imageOptionsField(name string) inference.FieldID {
	return inference.ExtensionField(name).Qualify(ImageOptions{})
}

func TestCompileImageQualityDrops(t *testing.T) {
	// Every canonical tier takes the same path: image-01 has no quality
	// parameter, so the field is dropped with a reason rather than rejected,
	// whichever tier a newer canonical enum may add.
	for _, quality := range []media.ImageQuality{
		media.ImageQualityAuto,
		media.ImageQualityLow,
		media.ImageQualityMedium,
		media.ImageQualityHigh,
		media.ImageQualityXHigh,
		media.ImageQualityMax,
	} {
		request := inference.GenerateRequest{
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: "a red circle"},
					}},
					Intent: inference.Intent{Image: &inference.ImageIntent{
						Quality: quality,
					}},
				},
			},
		}
		compiled, err := compileImage("ep-test")(
			context.Background(),
			model.ModelRef{
				ID: model.ModelID{Provider: providerID, Name: "image-01"},
			},
			request,
			inference.GenerateExecutionUnary,
		)
		if err != nil {
			t.Fatalf("quality %q: compile = %v, want quality dropped with success",
				quality, err)
		}
		found := false
		for _, decision := range compiled.Report.Decisions {
			if decision.Field == inference.FieldGenerateIntentImageQuality &&
				decision.Disposition == inference.Dropped &&
				strings.Contains(decision.Reason, "no quality parameter") {
				found = true
			}
		}
		if !found {
			t.Fatalf("quality %q: report decisions = %+v, want image-01 no-quality drop",
				quality, compiled.Report.Decisions)
		}
	}
}

func TestCompileImageAspectRatioExtension(t *testing.T) {
	request := imageRequestWithOptions(
		inference.ImageIntent{},
		ImageOptions{Provider: providerID, AspectRatio: "16:9"},
	)
	wire, report, err := compileImageWire(t, request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	if wire.aspectRatio != "16:9" || wire.width != 0 || wire.height != 0 {
		t.Fatalf("wire = %#v, want the ratio instead of dimensions", wire)
	}
	body := imageRequest(wire)
	if body["aspect_ratio"] != "16:9" {
		t.Errorf("body aspect_ratio = %v, want 16:9", body["aspect_ratio"])
	}
	if _, ok := body["width"]; ok {
		t.Errorf("body carries width alongside the ratio: %v", body)
	}
	found := false
	for _, decision := range report.Decisions {
		if decision.Field != imageOptionsField("aspect_ratio") {
			continue
		}
		found = true
		if decision.Disposition != inference.Native {
			t.Fatalf("aspect_ratio decision = %+v, want native", decision)
		}
	}
	if !found {
		t.Fatalf("report decisions = %+v, want the aspect_ratio extension field", report.Decisions)
	}
}

func TestCompileImageAspectRatioExtensionRejects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		intent inference.ImageIntent
		ratio  string
		reason string
	}{
		{
			name:   "size already selects the dimensions",
			intent: inference.ImageIntent{Size: &media.ImageSize{Width: 1024, Height: 1024}},
			ratio:  "16:9",
			reason: "canonical size",
		},
		{
			name:   "unsupported ratio",
			ratio:  "wide",
			reason: "aspect ratios are 1:1/16:9",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := imageRequestWithOptions(
				tc.intent,
				ImageOptions{Provider: providerID, AspectRatio: tc.ratio},
			)
			_, report, err := compileImageWire(t, request)
			if err == nil {
				t.Fatalf("compile unexpectedly succeeded; report = %+v", report)
			}
			reason := rejectedReason(report, imageOptionsField("aspect_ratio"))
			if !strings.Contains(reason, tc.reason) {
				t.Errorf("rejection reason = %q, want substring %q", reason, tc.reason)
			}
		})
	}
}

func TestImageOptionsValidate(t *testing.T) {
	if err := (ImageOptions{AspectRatio: "16:9"}).Validate(); err != nil {
		t.Errorf("known ratio Validate() = %v, want nil", err)
	}
	if err := (ImageOptions{AspectRatio: "wide"}).Validate(); err == nil {
		t.Error("unknown ratio was accepted")
	}
}
