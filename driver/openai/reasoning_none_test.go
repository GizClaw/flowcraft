package openai

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

func TestToggleFalseCompilesToEffortNone(t *testing.T) {
	compile := compileGenerate("gpt-5.6-sol", catalog["gpt-5.6-sol"])
	request := simpleTextRequest("hi")
	disabled := false
	request.Input.Content.Intent.Text.ReasoningEnabled = &disabled

	compiled, err := compile(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.reasoning != "none" {
		t.Fatalf("wire reasoning = %q, want none", compiled.Wire.reasoning)
	}
	if compiled.Report.Rejects(inference.FieldGenerateIntentReasoningEnabled) {
		t.Fatal("disable request unexpectedly rejected on an effort_none model")
	}
}

func TestToggleFalseRejectsWithoutEffortNone(t *testing.T) {
	entry := catalogEntry{
		kind:         kindGenerate,
		api:          apiResponses,
		capabilities: generateChatCapabilities().WithReasoning(inference.ReasoningToggle),
	}
	compile := compileGenerate("legacy-toggle", entry)
	request := simpleTextRequest("hi")
	disabled := false
	request.Input.Content.Intent.Text.ReasoningEnabled = &disabled

	compiled, err := compile(
		context.Background(),
		openaiModel("legacy-toggle"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatal("disable request unexpectedly accepted without effort_none")
	}
	if !compiled.Report.Rejects(inference.FieldGenerateIntentReasoningEnabled) {
		t.Fatal("disable request was not rejected on the reasoning_enabled field")
	}
}

func TestChatModeToggleFalseStillRejects(t *testing.T) {
	entry := catalogEntry{
		kind:         kindGenerate,
		api:          apiChat,
		capabilities: generateChatCapabilities().WithReasoning(inference.ReasoningToggle),
		effortNone:   true,
	}
	compile := compileGenerate("chat-toggle", entry)
	request := simpleTextRequest("hi")
	disabled := false
	request.Input.Content.Intent.Text.ReasoningEnabled = &disabled

	compiled, err := compile(
		context.Background(),
		openaiModel("chat-toggle"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatal("disable request unexpectedly accepted in chat mode")
	}
	if !compiled.Report.Rejects(inference.FieldGenerateIntentReasoningEnabled) {
		t.Fatal("disable request was not rejected in chat mode")
	}
}

func TestSpecToggleWithoutMapPassesEffortThrough(t *testing.T) {
	entry := catalogEntry{
		kind:         kindGenerate,
		api:          apiResponses,
		capabilities: generateChatCapabilities().WithReasoning(inference.ReasoningToggle),
	}
	compile := compileGenerate("spec-toggle", entry)
	request := simpleTextRequest("hi")
	request.Input.Content.Intent.Text.ReasoningEffort = inference.ReasoningHigh

	compiled, err := compile(
		context.Background(),
		openaiModel("spec-toggle"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.reasoning != "high" {
		t.Fatalf("wire reasoning = %q, want high", compiled.Wire.reasoning)
	}
	if compiled.Report.Dropped(inference.FieldGenerateIntentReasoningEffort) {
		t.Fatal("legacy spec effort unexpectedly dropped")
	}
}
