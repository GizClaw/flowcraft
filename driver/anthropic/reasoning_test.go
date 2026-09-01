package anthropic

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

func TestClaudeReasoningEffortResolvesAgainstPrivateDial(t *testing.T) {
	compile := compileGenerate("claude-fable-5", catalog["claude-fable-5"])
	model := conformanceModel("claude-fable-5")
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

func TestBinaryThinkingEffortDropsAndEnablesThinking(t *testing.T) {
	entry := catalogEntry{
		capabilities: generateChatCapabilities().
			WithReasoning(inference.ReasoningToggle),
	}
	compile := compileGenerate("binary", entry)
	request := conformanceTextRequest()
	request.Input.Content.Intent.Text = &inference.TextIntent{
		ReasoningEffort: inference.ReasoningHigh,
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
	if compiled.Wire.thinking == nil || !*compiled.Wire.thinking {
		t.Fatalf("binary drop must enable thinking, got %v", compiled.Wire.thinking)
	}
	if compiled.Wire.effort != "" {
		t.Fatalf("wire effort = %q, want empty", compiled.Wire.effort)
	}
}
