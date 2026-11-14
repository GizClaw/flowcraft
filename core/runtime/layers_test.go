package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/goccy/go-json"
)

func loadLayers(t *testing.T, layers ...Layer) (Config, Provenance, error) {
	t.Helper()
	return LoadLayers(context.Background(), layers)
}

// platformPaths rewrites the unix placeholders a fixture is written with, so the
// same document is absolute on both platforms: "/work" is not absolute on
// Windows, where a path needs a volume.
func platformPaths(text string) string {
	return strings.NewReplacer(
		"/work", pathtest.Abs("work"),
		"/tmp", pathtest.Abs("tmp"),
		"/expanded", pathtest.Abs("expanded"),
		"/fallback", pathtest.Abs("fallback"),
	).Replace(text)
}

func base() Layer {
	return Layer{Priority: 0, Name: "base.yaml", Bytes: []byte(platformPaths(`
version: 1
name: forge
sandbox:
  backend: local
  writes: [/work]
files:
  roots: ["/work"]
tools:
  enable: [files]
`))}
}

func override(body string) Layer {
	return Layer{Priority: 10, Name: "local.yaml", Bytes: []byte(body)}
}

func TestLoadLayersMergesSections(t *testing.T) {
	cfg, prov, err := loadLayers(t, base(), override(platformPaths(`
sandbox:
  writes: [/work, /tmp]
tools:
  tty: true
`)))
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	// A scalar the override did not mention survives.
	if cfg.Name != "forge" {
		t.Errorf("name = %q, want forge", cfg.Name)
	}
	if cfg.Sandbox.Backend != "local" {
		t.Errorf("backend = %q, want local", cfg.Sandbox.Backend)
	}
	// A sequence is replaced whole, not appended: one more line must not add a
	// writable path to the base list.
	wantWrites := pathtest.Abs("work") + "," + pathtest.Abs("tmp")
	if got := strings.Join(cfg.Sandbox.Writes, ","); got != wantWrites {
		t.Errorf("writes = %q, want the override list only", got)
	}
	if !cfg.Tools.TTY {
		t.Error("tty = false, want true from the override")
	}
	// Provenance names the layer that last set the value.
	if ref := prov["sandbox.writes"]; ref.Name != "local.yaml" || ref.Priority != 10 {
		t.Errorf("sandbox.writes provenance = %+v, want local.yaml/10", ref)
	}
	if ref := prov["name"]; ref.Name != "base.yaml" {
		t.Errorf("name provenance = %+v, want base.yaml", ref)
	}
}

func TestLoadLayersOperatesOnMappings(t *testing.T) {
	cfg, _, err := loadLayers(t, base(), override(`
sandbox:
  net:
    mode: allow_list
    allow_hosts: [api.example.com]
`))
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if cfg.Sandbox.Net.Mode != "allow_list" {
		t.Errorf("net.mode = %q", cfg.Sandbox.Net.Mode)
	}
}

func TestLoadLayersRejectsDuplicatePriority(t *testing.T) {
	_, _, err := loadLayers(t, base(), Layer{Priority: 0, Name: "other.yaml", Bytes: []byte("name: x")})
	if err == nil {
		t.Fatal("two layers with the same priority were accepted")
	}
	if !strings.Contains(err.Error(), "priority") {
		t.Errorf("error %q does not mention priority", err)
	}
}

// A typo in a permission section must not be silently ignored.
func TestLoadLayersRejectsUnknownKey(t *testing.T) {
	_, _, err := loadLayers(t, base(), override("sandbox:\n  writse: [/tmp]\n"))
	if err == nil {
		t.Fatal("unknown key accepted")
	}
	if !strings.Contains(err.Error(), "writse") {
		t.Errorf("error %q does not name the key", err)
	}
}

func TestLoadLayersExpandsEnv(t *testing.T) {
	root := pathtest.Abs("expanded")
	t.Setenv("RUNTIME_TEST_ROOT", root)
	cfg, _, err := loadLayers(t, Layer{Priority: 0, Name: "base.yaml", Bytes: []byte(platformPaths(`
version: 1
sandbox:
  backend: local
  workdir: ${RUNTIME_TEST_ROOT}
files:
  roots: ["${RUNTIME_TEST_ROOT}"]
`))})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if cfg.Files.Roots[0] != root || cfg.Sandbox.WorkDir != root {
		t.Errorf("roots=%v workdir=%q, want the expanded value", cfg.Files.Roots, cfg.Sandbox.WorkDir)
	}
}

// An unset variable with no fallback is an error: an empty value becomes a
// relative path, and the failure would then point at the wrong field.
func TestLoadLayersRejectsUnsetEnv(t *testing.T) {
	_, _, err := loadLayers(t, Layer{Priority: 0, Name: "base.yaml", Bytes: []byte(`
version: 1
sandbox:
  backend: local
files:
  roots: ["${RUNTIME_TEST_ABSENT}"]
`)})
	if err == nil {
		t.Fatal("unset variable accepted")
	}
	if !strings.Contains(err.Error(), "RUNTIME_TEST_ABSENT") {
		t.Errorf("error %q does not name the variable", err)
	}
}

func TestLoadLayersFallback(t *testing.T) {
	cfg, _, err := loadLayers(t, Layer{Priority: 0, Name: "base.yaml", Bytes: []byte(platformPaths(`
version: 1
sandbox:
  backend: local
files:
  roots: ["${RUNTIME_TEST_ABSENT:-/fallback}"]
`))})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if cfg.Files.Roots[0] != pathtest.Abs("fallback") {
		t.Errorf("roots = %v, want the fallback", cfg.Files.Roots)
	}
}

func TestLoadLayersFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "base.yaml")
	body := "version: 1\nsandbox:\n  backend: local\nfiles:\n  roots: [" + t.TempDir() + "]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write layer: %v", err)
	}
	if _, _, err := loadLayers(t, Layer{Priority: 0, Name: "base.yaml", Path: path}); err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
}

// A commented-out example must not be able to break a working configuration:
// expansion runs on the parsed tree, so comments are already gone.
func TestLoadLayersIgnoresCommentedVariables(t *testing.T) {
	cfg, _, err := loadLayers(t, Layer{Priority: 0, Name: "base.yaml", Bytes: []byte(platformPaths(`
version: 1
sandbox:
  backend: local
files:
  roots: ["/work"]
# mcp:
#   servers:
#     - env: { GREETING: "${TOTALLY_UNSET}" }
`))})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if cfg.Files.Roots[0] != pathtest.Abs("work") {
		t.Errorf("roots = %v", cfg.Files.Roots)
	}
}

// A "#" inside a quoted value is data, not a comment, and still expands.
func TestLoadLayersExpandsInsideQuotedValues(t *testing.T) {
	t.Setenv("RUNTIME_TEST_HOST", "api.example.test")
	cfg, _, err := loadLayers(t, Layer{Priority: 0, Name: "base.yaml", Bytes: []byte(platformPaths(`
version: 1
sandbox:
  backend: local
files:
  roots: ["/work"]
inference:
  providers:
    - name: primary
      driver: stub
      settings:
        endpoint: "https://${RUNTIME_TEST_HOST}/v1#fragment"
  models:
    - provider: primary
      model: m
`))})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(cfg.Inference.Providers[0].Settings, &settings); err != nil {
		t.Fatalf("settings: %v", err)
	}
	if got := settings["endpoint"]; got != "https://api.example.test/v1#fragment" {
		t.Errorf("endpoint = %v", got)
	}
}
