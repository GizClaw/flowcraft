package bytedance

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	arkresponses "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model/responses"
)

func TestDoubaoReasoningEffortResolvesAgainstPrivateDial(t *testing.T) {
	compile := compileGenerate("doubao-seed-2-1-pro", catalog["doubao-seed-2-1-pro"])
	ref := conformanceModel("doubao-seed-2-1-pro")
	field := inference.FieldGenerateIntentReasoningEffort

	for _, tc := range []struct {
		effort  model.ReasoningEffort
		want    model.ReasoningEffort
		dropped bool
	}{
		{effort: model.ReasoningMinimal, want: model.ReasoningLow, dropped: true},
		{effort: model.ReasoningLow, want: model.ReasoningLow},
		{effort: model.ReasoningMedium, want: model.ReasoningMedium},
		{effort: model.ReasoningHigh, want: model.ReasoningHigh},
		{effort: model.ReasoningXHigh, want: model.ReasoningHigh, dropped: true},
	} {
		request := conformanceTextRequest()
		request.Input.Content.Intent.Text = &inference.TextIntent{
			ReasoningEffort: tc.effort,
		}
		compiled, err := compile(
			context.Background(),
			ref,
			request,
			inference.GenerateExecutionUnary,
		)
		if err != nil {
			t.Fatalf("effort %q: compile: %v", tc.effort, err)
		}
		reasoning := compiled.Wire.GetReasoning()
		if reasoning == nil ||
			reasoning.GetEffort() != arkReasoningEffort(string(tc.want)) {
			t.Fatalf(
				"effort %q: wire = %+v, want %q",
				tc.effort,
				reasoning,
				tc.want,
			)
		}
		if got := compiled.Report.Dropped(field); got != tc.dropped {
			t.Fatalf("effort %q: dropped = %v, want %v", tc.effort, got, tc.dropped)
		}
	}
}

func TestSpecBinaryReasoningEffortDropsAndEnablesThinking(t *testing.T) {
	entry := catalogEntry{
		kind:         kindGenerate,
		capabilities: generateChatCapabilities().WithReasoning(model.ReasoningToggle),
	}
	compile := compileGenerate("spec-binary", entry)
	request := conformanceTextRequest()
	request.Input.Content.Intent.Text = &inference.TextIntent{
		ReasoningEffort: model.ReasoningHigh,
	}
	compiled, err := compile(
		context.Background(),
		conformanceModel("spec-binary"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !compiled.Report.Dropped(inference.FieldGenerateIntentReasoningEffort) {
		t.Fatal("binary thinking model must drop the effort with a reason")
	}
	thinking := compiled.Wire.GetThinking()
	if thinking == nil ||
		thinking.GetType() != arkresponses.ThinkingType_enabled {
		t.Fatalf("binary drop must enable thinking, got %v", thinking)
	}
	if compiled.Wire.GetReasoning() != nil {
		t.Fatalf("wire reasoning = %+v, want nil", compiled.Wire.GetReasoning())
	}
}
