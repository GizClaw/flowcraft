package qwen

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

func TestMaxPreviewReasoningEffortResolvesAgainstPrivateDial(t *testing.T) {
	compile := compileGenerate("qwen3.8-max-preview", catalog["qwen3.8-max-preview"])
	model := conformanceModel("qwen3.8-max-preview")
	field := inference.FieldGenerateIntentReasoningEffort

	for _, tc := range []struct {
		effort  inference.ReasoningEffort
		want    inference.ReasoningEffort
		dropped bool
	}{
		{effort: inference.ReasoningMinimal, want: inference.ReasoningLow, dropped: true},
		{effort: inference.ReasoningLow, want: inference.ReasoningLow},
		{effort: inference.ReasoningMedium, want: inference.ReasoningMedium},
		{effort: inference.ReasoningHigh, want: inference.ReasoningXHigh, dropped: true},
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
		if compiled.Wire.Parameters.ReasoningEffort != string(tc.want) {
			t.Fatalf(
				"effort %q: wire = %q, want %q",
				tc.effort,
				compiled.Wire.Parameters.ReasoningEffort,
				tc.want,
			)
		}
		if got := compiled.Report.Dropped(field); got != tc.dropped {
			t.Fatalf("effort %q: dropped = %v, want %v", tc.effort, got, tc.dropped)
		}
	}
}

func TestQwenBinaryThinkingDropsEffortWithoutEnabling(t *testing.T) {
	compile := compileGenerate("qwen3.7-max", catalog["qwen3.7-max"])
	request := conformanceTextRequest()
	request.Input.Content.Intent.Text = &inference.TextIntent{
		ReasoningEffort: inference.ReasoningHigh,
	}
	compiled, err := compile(
		context.Background(),
		conformanceModel("qwen3.7-max"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !compiled.Report.Dropped(inference.FieldGenerateIntentReasoningEffort) {
		t.Fatal("binary thinking model must drop the effort with a reason")
	}
	if compiled.Wire.Parameters.ReasoningEffort != "" {
		t.Fatalf(
			"wire effort = %q, want empty",
			compiled.Wire.Parameters.ReasoningEffort,
		)
	}
	if compiled.Wire.Parameters.EnableThinking != nil {
		t.Fatalf(
			"binary unary compile must not force stream-only thinking, got %v",
			*compiled.Wire.Parameters.EnableThinking,
		)
	}
}
