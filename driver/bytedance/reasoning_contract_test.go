package bytedance

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// TestDeclaredToggleKeepsItsEffortMap locks the declaration contract: the
// capabilities block is the whole fact, so a toggle that names an effort map
// keeps it, and the published toggle compiles reasoning_enabled=false.
func TestDeclaredToggleKeepsItsEffortMap(t *testing.T) {
	spec := decodeSpecWithModels(t, "", "doubao-seed-2-1-pro")
	models, err := resolveModelsForTest(t, spec)
	if err != nil {
		t.Fatalf("resolveModels: %v", err)
	}
	entry := models["doubao-seed-2-1-pro"]
	if entry.spec.Capabilities.Reasoning.Kind != model.ReasoningToggle {
		t.Fatalf("declaration must state reasoning toggle, got %q",
			entry.spec.Capabilities.Reasoning.Kind)
	}
	if len(entry.spec.Capabilities.Reasoning.EffortMap) == 0 {
		t.Fatal("declaration must state the effort map")
	}
	compiled, err := compileGenerateFor("doubao-seed-2-1-pro", entry)(
		context.Background(),
		conformanceModel("doubao-seed-2-1-pro"),
		inferencetest.ReasoningOffProbe(),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("toggle model rejected reasoning off: %v", err)
	}
	if compiled.Report.Rejects(inference.FieldGenerateIntentReasoningEnabled) {
		t.Fatal("toggle model rejected on the reasoning_enabled field")
	}
}

// TestVideoParamsDeclarationRoundTrip locks the control-fact vocabulary a
// video declaration carries: the deployment states the parameter matrix and
// the resolution cap, and the resolved model honors exactly that, with
// undeclared parameters left to the endpoint's own validation.
func TestVideoParamsDeclarationRoundTrip(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "my-seedance",
			"kind": "video",
			"capabilities": {
				"inputs": ["text", "image"],
				"outputs": ["video"]
			},
			"max_resolution": "1080p",
			"video": {
				"generate_audio": true,
				"duration_min_seconds": 4,
				"duration_max_seconds": 12,
				"reference_image": 2
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
	declared := models["my-seedance"].spec
	if declared.MaxResolution != "1080p" {
		t.Fatalf("max_resolution = %q, want 1080p", declared.MaxResolution)
	}
	video := declared.Video
	if !video.GenerateAudio || video.ReferenceImage != 2 ||
		video.DurationMin == nil || *video.DurationMin != 4 ||
		video.DurationMax == nil || *video.DurationMax != 12 {
		t.Fatalf("video params = %+v", video)
	}
	if video.Seed || video.Priority || video.OutputFormat {
		t.Fatalf("undeclared parameters appeared: %+v", video)
	}
}

// TestPublishedToggleCompilesReasoningOff is the generic conformance
// contract: every declared model that publishes toggle must compile the
// shared reasoning-off probe.
func TestPublishedToggleCompilesReasoningOff(t *testing.T) {
	spec := decodeSpecWithModels(t, "", fixtureNames...)
	models, err := resolveModelsForTest(t, spec)
	if err != nil {
		t.Fatalf("resolveModels: %v", err)
	}
	checked := 0
	for name, entry := range models {
		if modelKind(entry.spec.Kind) != kindGenerate ||
			entry.spec.Capabilities.Reasoning.Kind != model.ReasoningToggle {
			continue
		}
		checked++
		compiled, err := compileGenerateFor(name, entry)(
			context.Background(),
			conformanceModel(name),
			inferencetest.ReasoningOffProbe(),
			inference.GenerateExecutionUnary,
		)
		if err != nil {
			t.Fatalf("toggle model %q rejected reasoning off: %v", name, err)
		}
		if compiled.Report.Rejects(inference.FieldGenerateIntentReasoningEnabled) {
			t.Fatalf("toggle model %q rejected on the reasoning_enabled field", name)
		}
	}
	if checked == 0 {
		t.Fatal("no toggle model exercised")
	}
}
