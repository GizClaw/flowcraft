package minimax

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// Test fixtures. The driver ships no model line-up, so the tests carry the
// declarations they exercise.
type testTarget struct {
	spec ModelSpec
}

// declarations holds the models the tests compile against. The names mirror
// the models the driver used to ship, so the assertions stay readable.
var declarations = map[string]testTarget{
	"speech-2.8-hd": {spec: ModelSpec{
		Name: "speech-2.8-hd",
		Kind: "tts",
		Capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithOutputs(message.PartAudio),
	}},
	"image-01": {spec: ModelSpec{
		Name: "image-01",
		Kind: "image",
		Capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartImage),
	}},
	"MiniMax-Hailuo-2.3": {spec: ModelSpec{
		Name: "MiniMax-Hailuo-2.3",
		Kind: "video",
		Capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		Video: VideoParams{TenSeconds: true, HD: true},
	}},
	"MiniMax-Hailuo-2.3-Fast": {spec: ModelSpec{
		Name: "MiniMax-Hailuo-2.3-Fast",
		Kind: "video",
		Capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		Video: VideoParams{
			ImageToVideoOnly: true,
			TenSeconds:       true,
			HD:               true,
		},
	}},
	"MiniMax-Hailuo-02": {spec: ModelSpec{
		Name: "MiniMax-Hailuo-02",
		Kind: "video",
		Capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		Video: VideoParams{
			TenSeconds: true,
			HD:         true,
			P512:       true,
			LastFrame:  true,
		},
	}},
	"MiniMax-H3": {spec: ModelSpec{
		Name: "MiniMax-H3",
		Kind: "video",
		Capabilities: model.ModelCapabilities{}.
			WithInputs(
				message.PartText,
				message.PartImage,
				message.PartVideo,
				message.PartAudio,
			).
			WithOutputs(message.PartVideo),
		Video: VideoParams{API: videoAPIV2},
	}},
	// H3-Context-IR deep-reads the multimodal context and returns an
	// enhanced video prompt (text), never a video. It rides the v2 task
	// API: same content roles as MiniMax-H3 video, duration 4-15s, and an
	// optional ratio; the target duration/ratio ride ContextIROptions.
	"MiniMax-H3-Context-IR": {spec: ModelSpec{
		Name:      "MiniMax-H3-Context-IR",
		Kind:      "context_ir",
		WireModel: "MiniMax-H3",
		Capabilities: model.ModelCapabilities{}.
			WithInputs(
				message.PartText,
				message.PartImage,
				message.PartVideo,
				message.PartAudio,
			).
			WithOutputs(message.PartText),
	}},
	// Music generation (text-to-music; music-cover stays out — see
	// music.go). The -free tiers are rate-limited gratis twins.
	"music-3.0": {spec: ModelSpec{
		Name: "music-3.0",
		Kind: "music",
		Capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithOutputs(message.PartAudio),
	}},
}

// fixtureNames lists every declaration in the fixture set.
var fixtureNames = []string{
	"MiniMax-H3",
	"MiniMax-H3-Context-IR",
	"MiniMax-Hailuo-02",
	"MiniMax-Hailuo-2.3",
	"MiniMax-Hailuo-2.3-Fast",
	"image-01",
	"music-3.0",
	"speech-2.8-hd",
}

// resolveModelsForTest mirrors what buildProvider does for one spec: validate
// every declaration against the family contract.
func resolveModelsForTest(
	t *testing.T,
	spec Spec,
) (map[string]testTarget, error) {
	t.Helper()
	resolved := make(map[string]testTarget, len(spec.Models))
	for _, declared := range spec.Models {
		if err := validateModel(declared); err != nil {
			return nil, err
		}
		resolved[declared.Name] = testTarget{spec: declared}
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

// compileVideoFor compiles one declared video model against the test
// endpoint.
func compileVideoFor(
	name string,
	target testTarget,
) inference.GenerateCompiler[videoWire] {
	return compileVideo("ep-test", target.spec)
}

// compileContextIRFor compiles one declared context_ir model against the test
// endpoint.
func compileContextIRFor(
	name string,
	target testTarget,
) inference.GenerateCompiler[contextIRWire] {
	return compileContextIR(wireModel(name, target.spec), target.spec)
}
