package openai

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
)

func TestToggleFalseCompilesToWireNone(t *testing.T) {
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
		t.Fatal("disable request unexpectedly rejected on a toggle model")
	}
}

func TestChatModeToggleFalseStillRejects(t *testing.T) {
	entry := catalogEntry{
		kind: kindGenerate,

		capabilities: generateChatCapabilities().WithReasoning(inference.ReasoningToggle),
		dialect: dialect{
			api: apiChat,
		}}
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
		kind: kindGenerate,

		capabilities: generateChatCapabilities().WithReasoning(inference.ReasoningToggle),
		dialect: dialect{
			api: apiResponses,
		}}
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

// TestRedeclaredBuiltinKeepsToggleRoute locks the original bug: redeclaring
// gpt-5.6-sol to tweak one channel must keep the built-in reasoning kind and
// effort map, so the published toggle keeps compiling
// reasoning_enabled=false without restating the whole declaration.
func TestRedeclaredBuiltinKeepsToggleRoute(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "gpt-5.6-sol",
			"kind": "generate",
			"capabilities": {
				"inputs": ["text", "image", "data", "tool_call", "tool_result"],
				"outputs": ["text"],
				"hosted_web_search": true,
				"reasoning": {"kind": "toggle"}
			}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	entry := models["gpt-5.6-sol"]
	if entry.capabilities.Reasoning.Kind != inference.ReasoningToggle {
		t.Fatalf("reasoning kind = %q, want toggle", entry.capabilities.Reasoning.Kind)
	}
	if len(entry.capabilities.Reasoning.EffortMap) != 5 {
		t.Fatalf("redeclaration must keep the built-in effort map, got %v",
			entry.capabilities.Reasoning.EffortMap)
	}

	compiled, err := compileGenerate("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		inferencetest.ReasoningOffProbe(),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("disable request rejected after redeclaration: %v", err)
	}
	if compiled.Wire.reasoning != "none" {
		t.Fatalf("wire reasoning = %q, want none", compiled.Wire.reasoning)
	}
	if compiled.Report.Rejects(inference.FieldGenerateIntentReasoningEnabled) {
		t.Fatal("disable request unexpectedly rejected after redeclaration")
	}
}

func TestRedeclaredBuiltinWithoutCapabilitiesInheritsWholesale(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{"name": "gpt-5.6-sol", "kind": "generate"}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	entry := models["gpt-5.6-sol"]
	builtin := catalog["gpt-5.6-sol"]
	in, out := entry.limits.Values()
	bin, bout := builtin.limits.Values()
	if in != bin || out != bout {
		t.Fatalf("limits = %d/%d, want built-in %d/%d",
			in, out, bin, bout)
	}
	if entry.capabilities.Reasoning.Kind != inference.ReasoningToggle ||
		len(entry.capabilities.Reasoning.EffortMap) != 5 {
		t.Fatalf("empty capabilities block must inherit reasoning wholesale: %+v",
			entry.capabilities.Reasoning)
	}
}

// TestCustomToggleCompilesReasoningOff locks the contract's custom-model
// side: on the Responses surface "toggle" means the deployment asserts the
// endpoint honors reasoning.effort="none", so no separate declaration is
// needed and the switch must compile.
func TestCustomToggleCompilesReasoningOff(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "my-toggle",
			"kind": "generate",
			"capabilities": {
				"outputs": ["text"],
				"reasoning": {"kind": "toggle"}
			}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	compiled, err := compileGenerate("my-toggle", models["my-toggle"])(
		context.Background(),
		openaiModel("my-toggle"),
		inferencetest.ReasoningOffProbe(),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("custom toggle rejected reasoning off: %v", err)
	}
	if compiled.Wire.reasoning != "none" {
		t.Fatalf("wire reasoning = %q, want none", compiled.Wire.reasoning)
	}
}

// TestRedeclaredBuiltinAsAlwaysGivesTheMigrationPath: a deployment whose
// endpoint cannot disable reasoning declares kind always and keeps every
// other built-in fact without restating it.
func TestRedeclaredBuiltinAsAlwaysGivesTheMigrationPath(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "gpt-5.6-sol",
			"kind": "generate",
			"capabilities": {"outputs": ["text"], "reasoning": {"kind": "always"}}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	entry := models["gpt-5.6-sol"]
	if entry.capabilities.Reasoning.Kind != inference.ReasoningAlways {
		t.Fatalf("reasoning kind = %q, want always", entry.capabilities.Reasoning.Kind)
	}
	if len(entry.capabilities.Reasoning.EffortMap) != 5 {
		t.Fatalf("reasoning effort map must still inherit, got %v",
			entry.capabilities.Reasoning.EffortMap)
	}
	if _, err := compileGenerate("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		inferencetest.ReasoningOffProbe(),
		inference.GenerateExecutionUnary,
	); err == nil {
		t.Fatal("always model unexpectedly compiled reasoning_enabled=false")
	}
}

// TestChatSurfacePublishesAlways locks the per-surface materialization
// contract: on api: chat no model may publish toggle, because the wire
// cannot express reasoning off there.
func TestChatSurfacePublishesAlways(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{"api": "chat"}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	for name, entry := range models {
		if entry.kind != kindGenerate || entry.dialect.api != apiChat {
			continue
		}
		if entry.capabilities.Reasoning.Kind == inference.ReasoningToggle {
			t.Fatalf("model %q publishes toggle on the chat surface", name)
		}
		if entry.capabilities.Reasoning.Kind != inference.ReasoningAlways {
			continue
		}
		compiled, err := compileGenerate(name, entry)(
			context.Background(),
			openaiModel(name),
			inferencetest.ReasoningOffProbe(),
			inference.GenerateExecutionUnary,
		)
		if err == nil {
			t.Fatalf("model %q compiled reasoning_enabled=false on chat", name)
		}
		if !compiled.Report.Rejects(inference.FieldGenerateIntentReasoningEnabled) {
			t.Fatalf("model %q did not reject on the reasoning_enabled field", name)
		}
	}
}

// TestPublishedToggleCompilesReasoningOff is the generic conformance
// contract: every generate model that publishes toggle on the responses
// surface must compile the shared reasoning-off probe.
func TestPublishedToggleCompilesReasoningOff(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	checked := 0
	for name, entry := range models {
		if entry.kind != kindGenerate ||
			entry.capabilities.Reasoning.Kind != inference.ReasoningToggle {
			continue
		}
		checked++
		if compiled, err := compileGenerate(name, entry)(
			context.Background(),
			openaiModel(name),
			inferencetest.ReasoningOffProbe(),
			inference.GenerateExecutionUnary,
		); err != nil {
			t.Fatalf("toggle model %q rejected reasoning off: %v", name, err)
		} else if compiled.Report.Rejects(
			inference.FieldGenerateIntentReasoningEnabled,
		) {
			t.Fatalf("toggle model %q rejected on the reasoning_enabled field", name)
		}
	}
	if checked == 0 {
		t.Fatal("no toggle model exercised")
	}
}

// TestLegacyEffortNoneConfigNowRejected documents the effort_none removal:
// strict spec decoding must surface the key as unknown instead of silently
// accepting a knob that no longer exists.
func TestLegacyEffortNoneConfigNowRejected(t *testing.T) {
	if _, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "gpt-5.6-sol",
			"kind": "generate",
			"capabilities": {"outputs": ["text"], "reasoning": {"kind": "toggle"}},
			"effort_none": true
		}]
	}`)); err == nil {
		t.Fatal("spec with effort_none unexpectedly accepted")
	}
}

// TestChatSurfaceLowersDeclaredToggle guards the per-surface resolution for
// spec-declared models too: even an explicit toggle declaration on api:
// chat must publish always, because the wire cannot express reasoning off.
func TestChatSurfaceLowersDeclaredToggle(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"api": "chat",
		"models": [{
			"name": "gpt-5.6-sol",
			"kind": "generate",
			"capabilities": {"outputs": ["text"], "reasoning": {"kind": "toggle"}}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	entry := models["gpt-5.6-sol"]
	if entry.capabilities.Reasoning.Kind != inference.ReasoningAlways {
		t.Fatalf("chat redeclaration reasoning kind = %q, want always",
			entry.capabilities.Reasoning.Kind)
	}
	if _, err := compileGenerate("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		inferencetest.ReasoningOffProbe(),
		inference.GenerateExecutionUnary,
	); err == nil {
		t.Fatal("chat toggle unexpectedly compiled reasoning_enabled=false")
	}
}
