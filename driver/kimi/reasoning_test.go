package kimi

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

func TestK3ReasoningEffortResolvesAgainstPrivateDial(t *testing.T) {
	compile := compileGenerate("kimi-k3", catalog["kimi-k3"])
	model := conformanceModel("kimi-k3")
	field := inference.FieldGenerateIntentReasoningEffort

	for _, tc := range []struct {
		effort  inference.ReasoningEffort
		want    inference.ReasoningEffort
		dropped bool
	}{
		{effort: inference.ReasoningMinimal, want: inference.ReasoningLow, dropped: true},
		{effort: inference.ReasoningLow, want: inference.ReasoningLow},
		{effort: inference.ReasoningMedium, want: inference.ReasoningHigh, dropped: true},
		{effort: inference.ReasoningHigh, want: inference.ReasoningHigh},
		{effort: inference.ReasoningXHigh, want: "max", dropped: true},
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
		if compiled.Wire.Effort != string(tc.want) {
			t.Fatalf(
				"effort %q: wire = %q, want %q",
				tc.effort,
				compiled.Wire.Effort,
				tc.want,
			)
		}
		if got := compiled.Report.Dropped(field); got != tc.dropped {
			t.Fatalf("effort %q: dropped = %v, want %v", tc.effort, got, tc.dropped)
		}
	}
}

func TestK2xReasoningEffortDropsOnBinaryThinking(t *testing.T) {
	compile := compileGenerate("kimi-k2.6", catalog["kimi-k2.6"])
	request := conformanceTextRequest()
	request.Input.Content.Intent.Text = &inference.TextIntent{
		ReasoningEffort: inference.ReasoningHigh,
	}
	compiled, err := compile(
		context.Background(),
		conformanceModel("kimi-k2.6"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !compiled.Report.Dropped(inference.FieldGenerateIntentReasoningEffort) {
		t.Fatal("binary thinking model must drop the effort with a reason")
	}
	if compiled.Wire.Effort != "" {
		t.Fatalf("wire effort = %q, want empty", compiled.Wire.Effort)
	}
	if compiled.Wire.Thinking == nil || compiled.Wire.Thinking.Type != "enabled" {
		t.Fatalf("binary drop must enable thinking, got %+v", compiled.Wire.Thinking)
	}
}
