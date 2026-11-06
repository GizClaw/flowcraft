package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

type stubGenerator struct{}

func (stubGenerator) Generate(context.Context, inference.GenerateRequest) (inference.GenerateResponse, error) {
	return inference.GenerateResponse{}, nil
}

func (stubGenerator) GenerateStream(context.Context, inference.GenerateRequest) (inference.Stream, error) {
	return nil, nil
}

func localConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	return validConfig(root)
}

func TestNewAssemblesAndCloses(t *testing.T) {
	cfg := localConfig(t)
	rt, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if rt.ID() == "" {
		t.Error("runtime has no id")
	}
	if rt.Files() == nil || rt.Runner() == nil || rt.Tools() == nil {
		t.Fatalf("assembled %+v, want tree, runner and registry", rt)
	}
	if rt.Tools().Len() == 0 {
		t.Error("registry has no tools")
	}
	// No driver configured: a tools-only runtime is legitimate.
	if got := rt.Provider(); got.Generator != nil || got.Embedder != nil || got.Decider != nil {
		t.Errorf("provider = %+v, want the zero value", got)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

func TestNewRespectsToolSelection(t *testing.T) {
	cfg := localConfig(t)
	cfg.Tools.Enable = []string{ToolFiles}
	rt, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()
	names := rt.Tools().Names()
	if !contains(names, "read_file") {
		t.Errorf("tools = %v, want read_file", names)
	}
	if contains(names, "exec_session") || contains(names, "bash") {
		t.Errorf("tools = %v, want no exec tool", names)
	}
}

func TestNewRejectsUnknownDriverAndListsOffered(t *testing.T) {
	cfg := localConfig(t)
	cfg.Inference = inferenceConfig("nope", "primary", "m", nil)
	_, err := New(context.Background(), cfg,
		WithDrivers(Driver{Name: "alpha", Build: func(map[string]any) (inference.Provider, error) {
			return inference.Provider{Generator: stubGenerator{}}, nil
		}}))
	if err == nil {
		t.Fatal("unknown driver accepted")
	}
	if !strings.Contains(err.Error(), "alpha") {
		t.Errorf("error %q does not list the offered drivers", err)
	}
}

// A provider that declares nothing is rejected at construction, not at the first
// call.
func TestNewRejectsUncallableProvider(t *testing.T) {
	cfg := localConfig(t)
	cfg.Inference = inferenceConfig("empty", "primary", "m", nil)
	_, err := New(context.Background(), cfg,
		WithDrivers(Driver{Name: "empty", Build: func(map[string]any) (inference.Provider, error) {
			return inference.Provider{}, nil
		}}))
	if err == nil {
		t.Fatal("empty provider accepted")
	}
}

func TestNewBuildsDriverProvider(t *testing.T) {
	cfg := localConfig(t)
	cfg.Inference = inferenceConfig("stub", "primary", "gpt-6-luna", nil)
	settingsSeen := map[string]any{}
	rt, err := New(context.Background(), cfg,
		WithDrivers(Driver{Name: "stub", Build: func(settings map[string]any) (inference.Provider, error) {
			settingsSeen = settings
			return inference.Provider{Generator: stubGenerator{}}, nil
		}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()
	if rt.Provider().Generator == nil {
		t.Error("provider not wired")
	}
	if settingsSeen != nil {
		t.Errorf("settings = %v, want nil for an empty block", settingsSeen)
	}
	// The vendor is the driver name; the deployment identity is the profile.
	if got := rt.DefaultModel(); got.ID.Provider != "stub" || got.ID.Name != "gpt-6-luna" || got.Profile != "primary" {
		t.Errorf("default model = %+v, want stub/gpt-6-luna with profile primary", got)
	}
}

// A root that does not exist fails at construction: a handle that cannot be
// opened cannot enforce a boundary.
func TestNewRejectsMissingRoot(t *testing.T) {
	cfg := localConfig(t)
	cfg.Files.Roots = []string{cfg.Files.Roots[0] + "/absent"}
	cfg.Sandbox.Writes = cfg.Files.Roots
	if _, err := New(context.Background(), cfg); err == nil {
		t.Fatal("missing root accepted")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// Deferring hides the deferred tools from the first round and keeps the always
// ones visible; tool_search is registered by the assembly so the rest stays
// reachable.
func TestDynamicDefersToolsButKeepsAlwaysVisible(t *testing.T) {
	cfg := localConfig(t)
	cfg.Tools.Enable = []string{ToolFiles}
	cfg.Tools.Dynamic = &DynamicConfig{
		Default: "deferred",
		Always:  []string{"read_file"},
	}
	rt, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	visible := definitionNames(rt.NewCatalogView().Definitions())
	if !contains(visible, "read_file") {
		t.Errorf("visible = %v, want read_file", visible)
	}
	if !contains(visible, "tool_search") {
		t.Errorf("visible = %v, want tool_search registered", visible)
	}
	if contains(visible, "edit") {
		t.Errorf("visible = %v, want edit deferred", visible)
	}
	// The tool exists: deferral hides it, it does not remove it.
	if _, ok := rt.Tools().Get("edit"); !ok {
		t.Error("edit is absent from the registry, want it deferred only")
	}
}

// Without tools.dynamic every wired tool is visible from the first round.
func TestStaticExposesEveryTool(t *testing.T) {
	cfg := localConfig(t)
	cfg.Tools.Enable = []string{ToolFiles}
	rt, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	visible := definitionNames(rt.NewCatalogView().Definitions())
	for _, name := range []string{"read_file", "write_file", "edit"} {
		if !contains(visible, name) {
			t.Errorf("visible = %v, missing %s", visible, name)
		}
	}
}

func definitionNames(definitions []message.ToolDefinition) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	return names
}

// A configured MCP server is deferred by default: it appears as one searchable
// placeholder, and declaring a command that does not exist does not fail
// assembly — only use does.
func TestMCPDeferredByDefault(t *testing.T) {
	cfg := localConfig(t)
	cfg.Tools.MCP = MCPConfig{Servers: []MCPServerConfig{{
		Name:        "gh",
		Transport:   "stdio",
		Command:     "/nonexistent/definitely-not-a-binary",
		Description: "GitHub issues and pull requests.",
	}}}
	rt, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	placeholder, ok := rt.Tools().Get("mcp:gh")
	if !ok {
		t.Fatalf("tools = %v, want the mcp:gh placeholder", rt.Tools().Names())
	}
	if !strings.Contains(placeholder.Definition().Description, "GitHub issues") {
		t.Errorf("placeholder description = %q", placeholder.Definition().Description)
	}
}

// inferenceConfig builds one provider block serving one model.
func inferenceConfig(driver, provider, modelName string, settings map[string]any) InferenceConfig {
	return InferenceConfig{
		Providers: []ProviderConfig{{Name: provider, Driver: driver, Settings: settings}},
		Models:    []ModelConfig{{Provider: provider, Model: modelName}},
	}
}
