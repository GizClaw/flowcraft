package openai

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

func TestToggleFalseCompilesToWireNone(t *testing.T) {
	compile := compileResponsesFor("gpt-5.6-sol", declarations["gpt-5.6-sol"])
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
	if string(compiled.Wire.params.Reasoning.Effort) != "none" {
		t.Fatalf("reasoning effort = %q, want none", compiled.Wire.params.Reasoning.Effort)
	}
	if compiled.Report.Rejects(inference.FieldGenerateIntentReasoningEnabled) {
		t.Fatal("disable request unexpectedly rejected on a toggle model")
	}
}

func TestChatModeToggleFalseStillRejects(t *testing.T) {
	entry := testTarget{
		kind: kindGenerate,
		spec: testSpec(kindGenerate, generateChatCapabilities().WithReasoning(model.ReasoningToggle)),
		dialect: dialect{
			surface: surfaceDialect{api: apiChat},
		},
	}
	compile := compileChatFor("chat-toggle", entry)
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
	entry := testTarget{
		kind: kindGenerate,
		spec: testSpec(kindGenerate, generateChatCapabilities().WithReasoning(model.ReasoningToggle)),
		dialect: dialect{
			surface: surfaceDialect{api: apiResponses},
		},
	}
	compile := compileResponsesFor("spec-toggle", entry)
	request := simpleTextRequest("hi")
	request.Input.Content.Intent.Text.ReasoningEffort = model.ReasoningHigh

	compiled, err := compile(
		context.Background(),
		openaiModel("spec-toggle"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if string(compiled.Wire.params.Reasoning.Effort) != "high" {
		t.Fatalf("reasoning effort = %q, want high", compiled.Wire.params.Reasoning.Effort)
	}
	if compiled.Report.Dropped(inference.FieldGenerateIntentReasoningEffort) {
		t.Fatal("legacy spec effort unexpectedly dropped")
	}
}

// TestDeclaredToggleKeepsItsEffortMap locks the declaration contract: the
// capabilities block is the whole fact, so a toggle that names an effort map
// keeps it, and the published toggle compiles reasoning_enabled=false on the
// Responses surface.
func TestDeclaredToggleKeepsItsEffortMap(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "my-toggle",
			"kind": "generate",
			"capabilities": {
				"inputs": ["text"],
				"outputs": ["text"],
				"reasoning": {
					"kind": "toggle",
					"effort_map": {
						"minimal": "minimal",
						"low": "low",
						"medium": "medium",
						"high": "high",
						"xhigh": "xhigh"
					}
				}
			}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := resolveModelsForTest(t, spec)
	if err != nil {
		t.Fatalf("resolveModels: %v", err)
	}
	entry := models["my-toggle"]
	if entry.spec.Capabilities.Reasoning.Kind != model.ReasoningToggle {
		t.Fatalf("reasoning kind = %q, want toggle", entry.spec.Capabilities.Reasoning.Kind)
	}
	if len(entry.spec.Capabilities.Reasoning.EffortMap) != 5 {
		t.Fatalf("declaration must keep its effort map, got %v",
			entry.spec.Capabilities.Reasoning.EffortMap)
	}

	compiled, err := compileResponsesFor("my-toggle", entry)(
		context.Background(),
		openaiModel("my-toggle"),
		inferencetest.ReasoningOffProbe(),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("disable request rejected: %v", err)
	}
	if string(compiled.Wire.params.Reasoning.Effort) != "none" {
		t.Fatalf("reasoning effort = %q, want none", compiled.Wire.params.Reasoning.Effort)
	}
	if compiled.Report.Rejects(inference.FieldGenerateIntentReasoningEnabled) {
		t.Fatal("disable request unexpectedly rejected")
	}
}

// TestGenerateDeclarationMustStateTextOutput locks the other half of the
// declaration contract: there is no built-in fact to inherit, so a generate
// model that states no output is rejected at build time instead of silently
// publishing an empty promise.
func TestGenerateDeclarationMustStateTextOutput(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{"name": "understated", "kind": "generate"}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	_, err = resolveModelsForTest(t, spec)
	if err == nil {
		t.Fatal("generate model without a declared output was accepted")
	}
	if !strings.Contains(err.Error(), "text output") {
		t.Fatalf("error = %v, want the text-output contract", err)
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
	models, err := resolveModelsForTest(t, spec)
	if err != nil {
		t.Fatalf("resolveModels: %v", err)
	}
	compiled, err := compileResponsesFor("my-toggle", models["my-toggle"])(
		context.Background(),
		openaiModel("my-toggle"),
		inferencetest.ReasoningOffProbe(),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("custom toggle rejected reasoning off: %v", err)
	}
	if string(compiled.Wire.params.Reasoning.Effort) != "none" {
		t.Fatalf("reasoning effort = %q, want none", compiled.Wire.params.Reasoning.Effort)
	}
}

// TestDeclaredAlwaysRejectsReasoningOff: a deployment whose endpoint cannot
// disable reasoning declares kind always, and the compiler refuses the
// reasoning-off probe instead of silently ignoring the switch.
func TestDeclaredAlwaysRejectsReasoningOff(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "always-on",
			"kind": "generate",
			"capabilities": {"outputs": ["text"], "reasoning": {"kind": "always"}}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := resolveModelsForTest(t, spec)
	if err != nil {
		t.Fatalf("resolveModels: %v", err)
	}
	entry := models["always-on"]
	if entry.spec.Capabilities.Reasoning.Kind != model.ReasoningAlways {
		t.Fatalf("reasoning kind = %q, want always", entry.spec.Capabilities.Reasoning.Kind)
	}
	if _, err := compileResponsesFor("always-on", entry)(
		context.Background(),
		openaiModel("always-on"),
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
	spec := decodeSpecWithModels(t, `"api": "chat"`, fixtureNames...)
	models, err := resolveModelsForTest(t, spec)
	if err != nil {
		t.Fatalf("resolveModels: %v", err)
	}
	for name, entry := range models {
		if entry.kind != kindGenerate || entry.dialect.surface.api != apiChat {
			continue
		}
		if entry.spec.Capabilities.Reasoning.Kind == model.ReasoningToggle {
			t.Fatalf("model %q publishes toggle on the chat surface", name)
		}
		if entry.spec.Capabilities.Reasoning.Kind != model.ReasoningAlways {
			continue
		}
		compiled, err := compileChatFor(name, entry)(
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
	spec := decodeSpecWithModels(t, "", fixtureNames...)
	models, err := resolveModelsForTest(t, spec)
	if err != nil {
		t.Fatalf("resolveModels: %v", err)
	}
	checked := 0
	for name, entry := range models {
		if entry.kind != kindGenerate ||
			entry.spec.Capabilities.Reasoning.Kind != model.ReasoningToggle {
			continue
		}
		checked++
		if compiled, err := compileResponsesFor(name, entry)(
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
	models, err := resolveModelsForTest(t, spec)
	if err != nil {
		t.Fatalf("resolveModels: %v", err)
	}
	entry := models["gpt-5.6-sol"]
	if entry.spec.Capabilities.Reasoning.Kind != model.ReasoningAlways {
		t.Fatalf("chat redeclaration reasoning kind = %q, want always",
			entry.spec.Capabilities.Reasoning.Kind)
	}
	if _, err := compileChatFor("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		inferencetest.ReasoningOffProbe(),
		inference.GenerateExecutionUnary,
	); err == nil {
		t.Fatal("chat toggle unexpectedly compiled reasoning_enabled=false")
	}
}
