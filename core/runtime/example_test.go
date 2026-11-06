package runtime_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/runtime/runctx"
)

// stubGenerator satisfies inference.Generator; the example never generates.
type stubGenerator struct{}

func (stubGenerator) Generate(context.Context, inference.GenerateRequest) (inference.GenerateResponse, error) {
	return inference.GenerateResponse{}, nil
}

func (stubGenerator) GenerateStream(context.Context, inference.GenerateRequest) (inference.Stream, error) {
	return nil, nil
}

// TestExampleYAMLBuildsARuntime runs the documented file end to end: layers,
// environment expansion, schema validation, driver resolution and assembly. An
// example that is never executed is a claim, not documentation.
func TestExampleYAMLBuildsARuntime(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".skills"), 0o755); err != nil {
		t.Fatalf("prepare skills root: %v", err)
	}
	t.Setenv("RUNTIME_ROOT", root)

	cfg, provenance, err := runtime.LoadLayers(context.Background(), []runtime.Layer{{
		Priority: 0, Name: "forge.yaml", Path: filepath.Join("testdata", "forge.yaml"),
	}})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if got := cfg.Files.Roots[0]; got != root {
		t.Errorf("files.roots = %q, want the expanded %q", got, root)
	}
	if ref, ok := provenance["sandbox.backend"]; !ok || ref.Name != "forge.yaml" {
		t.Errorf("provenance for sandbox.backend = %+v, %v", ref, ok)
	}

	var settings map[string]any
	rt, err := runtime.New(context.Background(), cfg, runtime.WithDrivers(runtime.Driver{
		Name: "openai",
		Build: func(s map[string]any) (inference.Provider, error) {
			settings = s
			return inference.Provider{Generator: stubGenerator{}}, nil
		},
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	if len(settings) != 0 {
		t.Errorf("settings = %v, want the opaque block passed through untouched", settings)
	}
	// The credential profile is a model entry field, not a driver setting: the
	// driver resolves it per request.
	if got := rt.DefaultModel(); got.Profile != "luna" || got.ID.Name != "gpt-6-luna" {
		t.Errorf("default model = %+v, want gpt-6-luna/luna", got)
	}
	for _, name := range []string{"read_file", "write_file", "edit", "bash", "exec_session", "skill_search"} {
		if _, ok := rt.Tools().Get(name); !ok {
			t.Errorf("tool %q not wired; got %v", name, rt.Tools().Names())
		}
	}
	// tools.dynamic defers everything except the always list, and the assembly
	// adds tool_search so the deferred set stays reachable.
	visible := map[string]bool{}
	for _, definition := range rt.NewCatalogView().Definitions() {
		visible[definition.Name] = true
	}
	if !visible["read_file"] || !visible["tool_search"] {
		t.Errorf("visible tools = %v, want read_file and tool_search", visible)
	}
	if visible["write_file"] {
		t.Errorf("visible tools = %v, want write_file deferred", visible)
	}
	// The runtime id reaches any context derived from it.
	ctx := rt.Context(context.Background())
	run, ok := runctx.From(ctx)
	if !ok || run.RuntimeID != rt.ID() {
		t.Errorf("run context = %+v, %v; want the runtime id", run, ok)
	}
}
