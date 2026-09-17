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
