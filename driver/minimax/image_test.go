package minimax

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func TestCompileImageQualityDrops(t *testing.T) {
	request := inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "a red circle"},
				}},
				Intent: inference.Intent{Image: &inference.ImageIntent{
					Quality: media.ImageQualityHigh,
				}},
			},
		},
	}
	compiled, err := compileImage("ep-test")(
		context.Background(),
		inference.ModelRef{
			ID: inference.ModelID{Provider: driverID, Name: "image-01"},
		},
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v, want quality dropped with success", err)
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
		t.Fatalf("report decisions = %+v, want image-01 no-quality drop",
			compiled.Report.Decisions)
	}
}
