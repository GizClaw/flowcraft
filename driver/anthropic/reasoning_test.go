package anthropic

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

func TestClaudeReasoningEffortResolvesAgainstPrivateDial(t *testing.T) {
	compile := compileGenerate("claude-fable-5", catalog["claude-fable-5"])
	ref := conformanceModel("claude-fable-5")
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
		{effort: model.ReasoningXHigh, want: model.ReasoningXHigh},
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
		if got := compiled.Wire.OutputConfig.Effort; string(got) != string(tc.want) {
			t.Fatalf(
				"effort %q: wire = %q, want %q",
				tc.effort,
				got,
				tc.want,
			)
		}
		if got := compiled.Report.Dropped(field); got != tc.dropped {
			t.Fatalf("effort %q: dropped = %v, want %v", tc.effort, got, tc.dropped)
		}
	}
}

func TestBinaryThinkingEffortDropsAndEnablesThinking(t *testing.T) {
	entry := catalogEntry{
		capabilities: generateChatCapabilities().
			WithReasoning(model.ReasoningToggle),
	}
	compile := compileGenerate("binary", entry)
	request := conformanceTextRequest()
	request.Input.Content.Intent.Text = &inference.TextIntent{
		ReasoningEffort: model.ReasoningHigh,
	}
	compiled, err := compile(
		context.Background(),
		conformanceModel("binary"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !compiled.Report.Dropped(inference.FieldGenerateIntentReasoningEffort) {
		t.Fatal("binary thinking model must drop the effort with a reason")
	}
	if compiled.Wire.Thinking.OfAdaptive == nil {
		t.Fatalf("binary drop must enable thinking, got %+v", compiled.Wire.Thinking)
	}
	if compiled.Wire.OutputConfig.Effort != "" {
		t.Fatalf("wire effort = %q, want empty", compiled.Wire.OutputConfig.Effort)
	}
}
