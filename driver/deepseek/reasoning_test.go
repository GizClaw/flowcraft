package deepseek

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

func TestDeepSeekReasoningEffortResolvesAgainstPrivateDial(t *testing.T) {
	compile := compileChatGenerate("deepseek-v4-flash", catalog["deepseek-v4-flash"])
	model := conformanceModel("deepseek-v4-flash")
	field := inference.FieldGenerateIntentReasoningEffort

	for _, tc := range []struct {
		effort  inference.ReasoningEffort
		want    inference.ReasoningEffort
		dropped bool
	}{
		{effort: inference.ReasoningMinimal, want: inference.ReasoningLow, dropped: true},
		{effort: inference.ReasoningLow, want: inference.ReasoningLow},
		{effort: inference.ReasoningMedium, want: inference.ReasoningMedium},
		{effort: inference.ReasoningHigh, want: inference.ReasoningHigh},
		{effort: inference.ReasoningXHigh, want: inference.ReasoningXHigh},
	} {
		request := conformanceTextRequest()
		request.Input.Content.Intent.Text = &inference.TextIntent{
			ReasoningEffort: tc.effort,
		}
		compiled, err := compile(
			context.Background(),
			model,
			request,
			inference.GenerateExecutionUnary,
		)
		if err != nil {
			t.Fatalf("effort %q: compile: %v", tc.effort, err)
		}
		if compiled.Wire.effort != string(tc.want) {
			t.Fatalf(
				"effort %q: wire = %q, want %q",
				tc.effort,
				compiled.Wire.effort,
				tc.want,
			)
		}
		if got := compiled.Report.Dropped(field); got != tc.dropped {
			t.Fatalf("effort %q: dropped = %v, want %v", tc.effort, got, tc.dropped)
		}
	}
}

func TestDeepSeekResponsesReasoningEffortResolvesAgainstPrivateDial(t *testing.T) {
	compile := compileResponsesGenerate("deepseek-v4-flash", catalog["deepseek-v4-flash"])
	model := conformanceModel("deepseek-v4-flash")
	field := inference.FieldGenerateIntentReasoningEffort

	for _, tc := range []struct {
		effort  inference.ReasoningEffort
		want    inference.ReasoningEffort
		dropped bool
	}{
		{effort: inference.ReasoningMinimal, want: inference.ReasoningLow, dropped: true},
		{effort: inference.ReasoningLow, want: inference.ReasoningLow},
		{effort: inference.ReasoningMedium, want: inference.ReasoningMedium},
		{effort: inference.ReasoningHigh, want: inference.ReasoningHigh},
		{effort: inference.ReasoningXHigh, want: inference.ReasoningXHigh},
	} {
		request := conformanceTextRequest()
		request.Input.Content.Intent.Text = &inference.TextIntent{
			ReasoningEffort: tc.effort,
		}
		compiled, err := compile(
			context.Background(),
			model,
			request,
			inference.GenerateExecutionUnary,
		)
		if err != nil {
			t.Fatalf("effort %q: compile: %v", tc.effort, err)
		}
		if compiled.Wire.reasoning != string(tc.want) {
			t.Fatalf(
				"effort %q: wire = %q, want %q",
				tc.effort,
				compiled.Wire.reasoning,
				tc.want,
			)
		}
		if got := compiled.Report.Dropped(field); got != tc.dropped {
			t.Fatalf("effort %q: dropped = %v, want %v", tc.effort, got, tc.dropped)
		}
	}
}

func TestChatSpecBinaryReasoningEffortDrops(t *testing.T) {
	compile := compileChatGenerate("spec-binary", chatEntry())
	request := conformanceTextRequest()
	request.Input.Content.Intent.Text = &inference.TextIntent{
		ReasoningEffort: inference.ReasoningHigh,
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
		t.Fatal("binary model must drop the effort with a reason")
	}
	if compiled.Wire.effort != "" {
		t.Fatalf("wire effort = %q, want empty", compiled.Wire.effort)
	}
}
