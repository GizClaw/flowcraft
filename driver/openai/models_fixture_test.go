package openai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/openai/openai-go/v3"
)

// Test fixtures. The driver ships no model line-up, so the tests carry the
// declarations they exercise: a target pairs the model's declared facts with
// the deployment wire policy the test wants to compile against.
type testTarget struct {
	kind    modelKind
	spec    ModelSpec
	dialect dialect
	scope   string
}

// defaultWire is the dialect a provider with no wire overrides speaks, i.e.
// the OpenAI defaults. Fixture targets are stamped with it the way
// buildProvider stamps a deployment's dialect onto every declaration, so a
// test compiles against what production would derive.
var defaultWire = Spec{}.dialect()

// chatWire is defaultWire addressed through the Chat Completions surface:
// the same OpenAI defaults, reached over the other generate API.
func chatWire() dialect {
	wire := defaultWire
	wire.surface.api = apiChat
	return wire
}

// generateChatCapabilities is the common declaration for the text
// chat/responses compiler family: text, structured data, and tool parts.
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

// openaiEffortMap is the canonical-to-wire identity map: OpenAI's
// reasoning.effort accepts the canonical five verbatim.
var openaiEffortMap = map[model.ReasoningEffort]string{
	model.ReasoningMinimal: string(model.ReasoningMinimal),
	model.ReasoningLow:     string(model.ReasoningLow),
	model.ReasoningMedium:  string(model.ReasoningMedium),
	model.ReasoningHigh:    string(model.ReasoningHigh),
	model.ReasoningXHigh:   string(model.ReasoningXHigh),
}

// testModel builds one declared target: the facts the tests exercise, with
// the OpenAI default wire policy and the scope the opener derives for the
// default profile.
func testModel(
	name string,
	kind modelKind,
	capabilities model.ModelCapabilities,
) testTarget {
	return testTarget{
		kind: kind,
		spec: ModelSpec{
			Name:         name,
			Kind:         string(kind),
			Capabilities: capabilities,
		},
		dialect: defaultWire,
		scope:   reasoningScopeFor(name),
	}
}

// resolveModelsForTest mirrors what buildProvider does for one spec: resolve
// each declared model, narrow it to the deployed surface, and validate the
// family contract. Tests that inspect resolution use it instead of building a
// whole provider.
func resolveModelsForTest(
	t *testing.T,
	spec Spec,
) (map[string]testTarget, error) {
	t.Helper()
	wire := spec.dialect()
	resolved := make(map[string]testTarget, len(spec.Models))
	for _, declared := range spec.Models {
		kind := modelKind(declared.Kind)
		if err := validateModel(kind, declared, wire); err != nil {
			return nil, err
		}
		resolved[declared.Name] = testTarget{
			kind:    kind,
			spec:    wire.narrowModel(kind, declared),
			dialect: wire,
		}
	}
	return resolved, nil
}

// testTargetWith takes one fixture declaration and compiles it against the
// deployment wire policy a surface test wants.
func testTargetWith(name string, wire dialect) testTarget {
	target := declarations[name]
	target.dialect = wire
	return target
}

// testSpec builds the declaration a test literal describes: the kind it names
// plus the capabilities it states (none when it states none).
func testSpec(kind modelKind, capabilities model.ModelCapabilities) ModelSpec {
	return ModelSpec{
		Kind:         string(kind),
		Capabilities: capabilities,
	}
}

// testLimits sets the declared limits of one target.
func (t testTarget) withLimits(limits model.ModelLimits) testTarget {
	t.spec.Limits = limits
	return t
}

// declarations holds the models the tests compile against. The names mirror
// the models the driver used to ship, so the assertions stay readable.
var declarations = map[string]testTarget{
	"gpt-5.6-sol": testModel(
		"gpt-5.6-sol", kindGenerate,
		generateChatCapabilities().
			WithInputs(message.PartImage).
			WithHostedWebSearch().
			WithReasoning(model.ReasoningToggle).
			WithReasoningEffortMap(openaiEffortMap),
	).withLimits(model.ModelLimits{}.
		WithMaxInputTokens(1_050_000).
		WithMaxOutputTokens(128_000)),
	"gpt-5.6-terra": testModel(
		"gpt-5.6-terra", kindGenerate,
		generateChatCapabilities().
			WithInputs(message.PartImage).
			WithHostedWebSearch().
			WithReasoning(model.ReasoningToggle).
			WithReasoningEffortMap(openaiEffortMap),
	).withLimits(model.ModelLimits{}.
		WithMaxInputTokens(1_050_000).
		WithMaxOutputTokens(128_000)),
	"gpt-4.1-nano": testModel(
		"gpt-4.1-nano", kindGenerate,
		generateChatCapabilities().WithInputs(message.PartImage),
	).withLimits(model.ModelLimits{}.
		WithMaxInputTokens(1_047_576).
		WithMaxOutputTokens(32_768)),
	"text-embedding-3-large": testModel(
		"text-embedding-3-large", kindEmbed,
		model.ModelCapabilities{}.WithCustomEmbedDimensions(),
	).withLimits(model.ModelLimits{}.WithMaxInputTokens(8_192)),
	"text-embedding-ada-002": testModel(
		"text-embedding-ada-002", kindEmbed,
		model.ModelCapabilities{},
	).withLimits(model.ModelLimits{}.WithMaxInputTokens(8_192)),
}

// fixtureNames lists every declaration in the fixture set.
var fixtureNames = []string{
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-4.1-nano",
	"text-embedding-3-large",
	"text-embedding-ada-002",
}

// fixtureModelsJSON renders the named fixture declarations as the models
// array a deployment spec carries, so a test decoding a spec states the
// declarations it exercises without restating them.
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
func compileChatFor(name string, target testTarget) inference.GenerateCompiler[*chatRequest] {
	return compileChat(name, target.spec, target.dialect, target.scope)
}

func compileResponsesFor(
	name string,
	target testTarget,
) inference.GenerateCompiler[*responsesRequest] {
	return compileResponses(name, target.spec, target.dialect, target.scope)
}

func newChatRequestFor(
	name string,
	target testTarget,
	shape inference.GenerateExecutionShape,
) *chatRequest {
	return newChatRequest(name, target.spec, target.dialect, shape)
}

func newResponsesRequestFor(name string, target testTarget) *responsesRequest {
	return newResponsesRequest(name, target.spec, target.dialect)
}

func openGenerateFor(
	cls *clients,
	target testTarget,
	id model.ModelID,
	profile string,
) (inference.GenerateOperations, error) {
	return openGenerate(cls, target.spec, target.dialect, id, profile)
}

func compileEmbedFor(
	name string,
	target testTarget,
) inference.Compiler[inference.EmbedRequest, openai.EmbeddingNewParams] {
	return compileEmbed(name, target.spec, target.dialect)
}

func compileGenerateTuningFor(
	sink generateSink,
	options GenerateOptions,
	target testTarget,
	ledger *inference.Ledger,
) {
	compileGenerateTuning(sink, options, target.spec, target.dialect, ledger)
}
