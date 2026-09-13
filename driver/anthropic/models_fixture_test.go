package anthropic

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	anthropicgo "github.com/anthropics/anthropic-sdk-go"
)

// Test fixtures. The driver ships no model line-up, so the tests carry the
// declarations they exercise: a target pairs the model's declared facts with
// the provider wire policy the test wants to compile against.
type testTarget struct {
	spec  ModelSpec
	wire  dialect
	scope string
}

// defaultWire is the dialect a provider with no wire extensions speaks.
var defaultWire = Spec{}.dialect()

// generateChatCapabilities is the common declaration for the Claude text
// compiler family.
func generateChatCapabilities() model.ModelCapabilities {
	return model.ModelCapabilities{
		Inputs: []message.PartKind{
			message.PartText,
			message.PartData,
			message.PartToolCall,
			message.PartToolResult,
		},
		Outputs: []message.PartKind{message.PartText},
	}
}

// claudeEffortMap is the canonical-to-wire effort map shared by every Claude
// model that accepts effort levels, aligned with the adaptive-thinking effort
// parameter (low/medium/high/xhigh; "max" is a model-side extra beyond the
// canonical ladder and is never mapped to).
var claudeEffortMap = map[model.ReasoningEffort]string{
	model.ReasoningMinimal: string(model.ReasoningLow),
	model.ReasoningLow:     string(model.ReasoningLow),
	model.ReasoningMedium:  string(model.ReasoningMedium),
	model.ReasoningHigh:    string(model.ReasoningHigh),
	model.ReasoningXHigh:   string(model.ReasoningXHigh),
}

// testModel builds one declared target: the facts the tests exercise, with
// the default wire policy.
func testModel(
	name string,
	capabilities model.ModelCapabilities,
	limits model.ModelLimits,
) testTarget {
	return testTarget{
		spec: ModelSpec{
			Name:         name,
			Kind:         "generate",
			Capabilities: capabilities,
			Limits:       limits,
		},
		wire: defaultWire,
	}
}

// declarations holds the models the tests compile against. The names mirror
// the models the driver used to ship, so the assertions stay readable.
var declarations = map[string]testTarget{
	"claude-fable-5": testModel(
		"claude-fable-5",
		generateChatCapabilities().
			WithInputs(message.PartImage).
			WithReasoning(model.ReasoningAlways).
			WithReasoningEffortMap(claudeEffortMap),
		model.ModelLimits{}.
			WithMaxInputTokens(1_000_000).
			WithMaxOutputTokens(128_000),
	),
	"claude-opus-5": testModel(
		"claude-opus-5",
		generateChatCapabilities().
			WithInputs(message.PartImage).
			WithReasoning(model.ReasoningToggle).
			WithReasoningEffortMap(claudeEffortMap),
		model.ModelLimits{}.
			WithMaxInputTokens(1_000_000).
			WithMaxOutputTokens(128_000),
	),
	"claude-sonnet-5": testModel(
		"claude-sonnet-5",
		generateChatCapabilities().
			WithInputs(message.PartImage).
			WithReasoning(model.ReasoningToggle).
			WithReasoningEffortMap(claudeEffortMap),
		model.ModelLimits{}.
			WithMaxInputTokens(1_000_000).
			WithMaxOutputTokens(128_000),
	),
}

// fixtureNames lists every declaration in the fixture set.
var fixtureNames = []string{"claude-fable-5", "claude-opus-5", "claude-sonnet-5"}

// resolveModelsForTest mirrors what buildProvider does for one spec: validate
// every declaration against the family contract. Tests that inspect resolution
// use it instead of building a whole provider.
func resolveModelsForTest(
	t *testing.T,
	spec Spec,
) (map[string]testTarget, error) {
	t.Helper()
	wire := spec.dialect()
	resolved := make(map[string]testTarget, len(spec.Models))
	for _, declared := range spec.Models {
		if err := validateModel(declared); err != nil {
			return nil, err
		}
		resolved[declared.Name] = testTarget{spec: declared, wire: wire}
	}
	return resolved, nil
}

// scopedEntry returns the target the opener hands to the compiler: the same
// declaration with the verification scope of one addressed model filled in.
func scopedEntry(entry testTarget, provider, name, profile string) testTarget {
	entry.scope = inference.ReasoningScope(
		entry.wire.scopeDeclared, provider, name, profile)
	return entry
}

// fixtureModelsJSON renders the named fixture declarations as the models array
// a deployment spec carries, so a test decoding a spec states the declarations
// it exercises without restating them.
func fixtureModelsJSON(t *testing.T, names ...string) string {
	t.Helper()
	declared := make([]ModelSpec, 0, len(names))
	for _, name := range names {
		target, ok := declarations[name]
		if !ok {
			t.Fatalf("fixture %q is not declared in the fixture set", name)
		}
		declared = append(declared, target.spec)
	}
	raw, err := json.Marshal(declared)
	if err != nil {
		t.Fatalf("marshal fixture declarations: %v", err)
	}
	return string(raw)
}

// decodeSpecWithModels decodes one spec document whose model declarations are
// the named fixtures: body is the rest of the JSON object, without its
// surrounding braces and with or without a trailing comma.
func decodeSpecWithModels(t *testing.T, body string, names ...string) Spec {
	t.Helper()
	doc := "{"
	if trimmed := strings.TrimSpace(body); trimmed != "" {
		doc += strings.TrimSuffix(trimmed, ",") + ","
	}
	doc += `"models":` + fixtureModelsJSON(t, names...) + "}"
	spec, err := decodeSpec(context.Background(), []byte(doc))
	if err != nil {
		t.Fatalf("decodeSpec(%s): %v", doc, err)
	}
	return spec
}

// Test call helpers: they keep the tests reading as "compile this model"
// while the production signatures take the declaration, the deployment wire
// policy, and the verification scope as separate values.
func compileGenerateFor(
	name string,
	target testTarget,
) inference.GenerateCompiler[anthropicgo.MessageNewParams] {
	return compileGenerate(name, target.spec, target.wire, target.scope)
}
