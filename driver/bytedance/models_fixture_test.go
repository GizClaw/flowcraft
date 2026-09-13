package bytedance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	arkresponses "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model/responses"

	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

// Test fixtures. The driver ships no model line-up, so the tests carry the
// declarations they exercise: a target pairs the model's declared facts with
// the provider wire policy the test compiles against.
type testTarget struct {
	spec ModelSpec
	wire dialect
}

// defaultWire is the dialect a provider with no scope declaration speaks.
var defaultWire = Spec{}.dialect()

// generateChatCapabilities is the common declaration for the Ark text
// compiler family. Individual fixtures add image/video input, hosted web
// search, and the reasoning control capability. Ark consumes no reasoning
// input, so PartReasoning is deliberately absent.
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

// arkEffortMap is the canonical-to-wire map shared by the Ark thinking
// models. The SDK's reasoning_effort enum carries low/medium/high only, so
// canonical minimal folds onto low and xhigh folds onto high; Doubao's
// documented minimal level is its no-thinking mode, which the canonical
// ReasoningEnabled switch covers instead.
var arkEffortMap = map[model.ReasoningEffort]string{
	model.ReasoningMinimal: string(model.ReasoningLow),
	model.ReasoningLow:     string(model.ReasoningLow),
	model.ReasoningMedium:  string(model.ReasoningMedium),
	model.ReasoningHigh:    string(model.ReasoningHigh),
	model.ReasoningXHigh:   string(model.ReasoningHigh),
}

// deprecated marks a fixture whose replacement is another fixture model.
func deprecated(replacement string) model.ModelLifecycle {
	return model.ModelLifecycle{
		Status: model.ModelStatusDeprecated,
		Replacement: &model.ModelID{
			Provider: "bytedance",
			Name:     replacement,
		},
	}
}

// videoSeconds builds the *int64 a fixture's duration bound carries.
func videoSeconds(value int64) *int64 { return &value }

// declarations holds the models the tests compile against. The names mirror
// the models the driver used to ship, so the assertions stay readable.
var declarations = map[string]testTarget{
	"doubao-seed-2-1-pro": {spec: ModelSpec{
		Name: "doubao-seed-2-1-pro",
		Kind: "generate",
		Capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithHostedWebSearch().
			WithReasoning(model.ReasoningToggle).
			WithReasoningEffortMap(arkEffortMap),
		Limits: model.ModelLimits{}.
			WithMaxInputTokens(256_000).
			WithMaxOutputTokens(256_000),
	}, wire: defaultWire},
	"doubao-seed-2-0-lite": {spec: ModelSpec{
		Name: "doubao-seed-2-0-lite",
		Kind: "generate",
		Capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo, message.PartAudio).
			WithHostedWebSearch().
			WithReasoning(model.ReasoningToggle).
			WithReasoningEffortMap(arkEffortMap),
		Limits: model.ModelLimits{}.
			WithMaxInputTokens(256_000).
			WithMaxOutputTokens(128_000),
	}, wire: defaultWire},
	"doubao-seed-2-0-mini": {spec: ModelSpec{
		Name: "doubao-seed-2-0-mini",
		Kind: "generate",
		Capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo, message.PartAudio).
			WithHostedWebSearch().
			WithReasoning(model.ReasoningToggle).
			WithReasoningEffortMap(arkEffortMap),
		Limits: model.ModelLimits{}.
			WithMaxInputTokens(256_000).
			WithMaxOutputTokens(128_000),
	}, wire: defaultWire},
	"doubao-embedding-large": {spec: ModelSpec{
		Name: "doubao-embedding-large",
		Kind: "embed",
		Capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithCustomEmbedDimensions(),
		Limits: model.ModelLimits{}.WithMaxInputTokens(4_095),
	}, wire: defaultWire},
	"doubao-embedding-vision": {spec: ModelSpec{
		Name: "doubao-embedding-vision",
		Kind: "embed",
		Capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithCustomEmbedDimensions(),
		Limits: model.ModelLimits{}.WithMaxInputTokens(8_191),
	}, wire: defaultWire},
	"doubao-seedance-2-5": {spec: ModelSpec{
		Name: "doubao-seedance-2-5",
		Kind: "video",
		Capabilities: model.ModelCapabilities{}.
			WithInputs(
				message.PartText,
				message.PartImage,
				message.PartVideo,
				message.PartAudio,
			).
			WithOutputs(message.PartVideo).
			WithHostedWebSearch(),
		MaxResolution: "1080p",
		Video: VideoParams{
			GenerateAudio:          true,
			Priority:               true,
			OutputFormat:           true,
			OmniReference:          true,
			DurationMin:            videoSeconds(4),
			DurationMax:            videoSeconds(30),
			DurationAuto:           true,
			AudioOnly:              true,
			FrameRatioAdaptiveOnly: true,
			ReferenceImage:         30,
			ReferenceVideo:         10,
			ReferenceAudio:         10,
		},
	}, wire: defaultWire},
	"doubao-seedance-2-0": {spec: ModelSpec{
		Name: "doubao-seedance-2-0",
		Kind: "video",
		Capabilities: model.ModelCapabilities{}.
			WithInputs(
				message.PartText,
				message.PartImage,
				message.PartVideo,
				message.PartAudio,
			).
			WithOutputs(message.PartVideo).
			WithHostedWebSearch(),
		MaxResolution: "4k",
		Video: VideoParams{
			GenerateAudio:  true,
			Priority:       true,
			DurationMin:    videoSeconds(4),
			DurationMax:    videoSeconds(15),
			DurationAuto:   true,
			ReferenceImage: 9,
			ReferenceVideo: 3,
			ReferenceAudio: 3,
		},
	}, wire: defaultWire},
	"doubao-seedance-2-0-fast": {spec: ModelSpec{
		Name: "doubao-seedance-2-0-fast",
		Kind: "video",
		Capabilities: model.ModelCapabilities{}.
			WithInputs(
				message.PartText,
				message.PartImage,
				message.PartVideo,
				message.PartAudio,
			).
			WithOutputs(message.PartVideo).
			WithHostedWebSearch(),
		MaxResolution: "720p",
		Video: VideoParams{
			GenerateAudio:  true,
			Priority:       true,
			DurationMin:    videoSeconds(4),
			DurationMax:    videoSeconds(15),
			DurationAuto:   true,
			ReferenceImage: 9,
			ReferenceVideo: 3,
			ReferenceAudio: 3,
		},
	}, wire: defaultWire},
	"doubao-seedance-1-5-pro": {spec: ModelSpec{
		Name: "doubao-seedance-1-5-pro",
		Kind: "video",
		Capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		MaxResolution: "1080p",
		Video: VideoParams{
			Seed:          true,
			CameraFixed:   true,
			FlexTier:      true,
			GenerateAudio: true,
			DurationMin:   videoSeconds(4),
			DurationMax:   videoSeconds(12),
			DurationAuto:  true,
		},
		Lifecycle: deprecated("doubao-seedance-2-0"),
	}, wire: defaultWire},
	"doubao-seedance-1-0-pro": {spec: ModelSpec{
		Name: "doubao-seedance-1-0-pro",
		Kind: "video",
		Capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		MaxResolution: "1080p",
		Video: VideoParams{
			Seed:        true,
			CameraFixed: true,
			FlexTier:    true,
			DurationMin: videoSeconds(2),
			DurationMax: videoSeconds(12),
		},
		Lifecycle: deprecated("doubao-seedance-2-0"),
	}, wire: defaultWire},
}

// fixtureNames lists every declaration in the fixture set.
var fixtureNames = []string{
	"doubao-embedding-large",
	"doubao-embedding-vision",
	"doubao-seed-2-0-lite",
	"doubao-seed-2-0-mini",
	"doubao-seed-2-1-pro",
	"doubao-seedance-1-0-pro",
	"doubao-seedance-1-5-pro",
	"doubao-seedance-2-0",
	"doubao-seedance-2-0-fast",
	"doubao-seedance-2-5",
}

// resolveModelsForTest mirrors what buildProvider does for one spec: validate
// every declaration against the family contract.
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

// fixtureModelsJSON renders the named fixture declarations as the models array
// a deployment spec carries.
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
// while the production signatures take the declaration and the deployment
// wire policy as separate values.
func compileGenerateFor(
	name string,
	target testTarget,
) inference.GenerateCompiler[*arkresponses.ResponsesRequest] {
	return compileGenerate(name, target.spec)
}

func compileEmbedFor(
	name string,
	target testTarget,
) inference.Compiler[inference.EmbedRequest, *embedRequest] {
	return compileEmbed(name, target.spec)
}

func compileVideoFor(
	name string,
	target testTarget,
) inference.GenerateCompiler[*arkmodel.CreateContentGenerationTaskRequest] {
	return compileVideo("ep-test", target.spec)
}
