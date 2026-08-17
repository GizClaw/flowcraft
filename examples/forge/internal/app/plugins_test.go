package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/examples/forge/internal/scenario"
)

// TestOpenMergesPluginLayers is the end-to-end host wiring check: a
// workspace deploy.yaml with a top-level plugins section is strictly
// parsed (section stripped first), the plugin loader merges the
// declaration layer over the document, and the runtime builds with the
// layer-contributed resource.
func TestOpenMergesPluginLayers(t *testing.T) {
	ref, err := scenario.Resolve("raids", "../../scenarios/raids/werewolf")
	if err != nil {
		t.Fatalf("resolve werewolf: %v", err)
	}
	dir := t.TempDir()
	if err := scenario.Copy(ref, dir); err != nil {
		t.Fatalf("copy scenario: %v", err)
	}

	// Add the plugins section to deploy.yaml and copy the demo plugin
	// into the workspace.
	raw, err := os.ReadFile(filepath.Join(dir, "deploy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte(`
plugins:
  dirs: [./plugins]
  enabled: [acme.hello-layer]
`)...)
	if err := os.WriteFile(filepath.Join(dir, "deploy.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	pluginDir := filepath.Join(dir, "plugins", "acme.hello-layer")
	if err := os.MkdirAll(filepath.Join(pluginDir, "layers"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(pluginDir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	copyFile("plugin.yaml", `
name: acme.hello-layer
version: 1.0.0
requires:
  core: ">=0.1.5"
artifacts:
  - type: layer
    path: layers/10-hello.yaml
    priority: 200
`)
	copyFile("layers/10-hello.yaml", `
resources:
  greeting:
    kind: event.Bus
    impl: memory
`)

	t.Setenv("DEEPSEEK_API_KEY", "sk-test")
	a, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = a.Close() }()
	if a.plugins == nil {
		t.Fatal("plugin set must be retained by the app")
	}
	if len(a.plugins.Layers) != 1 {
		t.Fatalf("plugin layers = %d, want 1", len(a.plugins.Layers))
	}
	if a.plugins.Layers[0].Priority != 200 {
		t.Fatalf("layer priority = %d, want 200", a.plugins.Layers[0].Priority)
	}
}

// TestInspectSkipsPluginsSection verifies the read-only path tolerates
// workspaces with a plugins section even before any plugin loads.
func TestInspectSkipsPluginsSection(t *testing.T) {
	ref, err := scenario.Resolve("raids", "../../scenarios/raids/werewolf")
	if err != nil {
		t.Fatalf("resolve werewolf: %v", err)
	}
	dir := t.TempDir()
	if err := scenario.Copy(ref, dir); err != nil {
		t.Fatalf("copy scenario: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "deploy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte(`
plugins:
  dirs: [./plugins]
  enabled: [acme.hello-layer]
`)...)
	if err := os.WriteFile(filepath.Join(dir, "deploy.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := Inspect(dir)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.AgentID != "assistant" {
		t.Fatalf("info = %+v", info)
	}
}
