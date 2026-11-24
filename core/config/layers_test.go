package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/goccy/go-json"
)

// testConfig is the shape these tests merge into: enough sections to exercise a
// scalar, a nested mapping, a sequence and an opaque settings block, and no
// more. The package itself knows none of it.
type testConfig struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	Sandbox struct {
		Backend string   `json:"backend"`
		WorkDir string   `json:"workdir"`
		Writes  []string `json:"writes"`
		Net     struct {
			Mode       string   `json:"mode"`
			AllowHosts []string `json:"allow_hosts"`
		} `json:"net"`
	} `json:"sandbox"`
	Files struct {
		Roots []string `json:"roots"`
	} `json:"files"`
	Inference struct {
		Providers []struct {
			Name     string          `json:"name"`
			Driver   string          `json:"driver"`
			Settings json.RawMessage `json:"settings"`
		} `json:"providers"`
		Models []struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"models"`
	} `json:"inference"`
	Tools struct {
		Enable []string `json:"enable"`
		TTY    bool     `json:"tty"`
	} `json:"tools"`
}

func load(t *testing.T, layers ...Layer) (testConfig, *Doc, error) {
	t.Helper()
	return Load[testConfig](context.Background(), layers, Strict[testConfig])
}

func mustLoad(t *testing.T, layers ...Layer) (testConfig, *Doc) {
	t.Helper()
	cfg, doc, err := load(t, layers...)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg, doc
}

// platformPaths rewrites the unix placeholders a fixture is written with, so the
// same document is absolute on both platforms: pathtest.Abs("work") is not absolute on
// Windows, where a path needs a volume.
func platformPaths(text string) string {
	return strings.NewReplacer(
		pathtest.Abs("work"), pathtest.Abs("work"),
		pathtest.Abs("tmp"), pathtest.Abs("tmp"),
		"/expanded", pathtest.Abs("expanded"),
		"/fallback", pathtest.Abs("fallback"),
	).Replace(text)
}

func base() Layer {
	return Layer{Priority: 0, Name: "base.yaml", Source: Bytes([]byte(platformPaths(`
version: 1
name: forge
sandbox:
  backend: local
  writes: [/work]
files:
  roots: ["/work"]
tools:
  enable: [files]
`)))}
}

func override(body string) Layer {
	return Layer{Priority: 10, Name: "local.yaml", Source: Bytes([]byte(body))}
}

func TestLoadMergesSections(t *testing.T) {
	cfg, doc := mustLoad(t, base(), override(platformPaths(`
sandbox:
  writes: [/work, /tmp]
tools:
  tty: true
`)))
	// A scalar the override did not mention survives.
	if cfg.Name != "forge" {
		t.Errorf("name = %q, want forge", cfg.Name)
	}
	if cfg.Sandbox.Backend != "local" {
		t.Errorf("backend = %q", cfg.Sandbox.Backend)
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
	if ref, ok := doc.Origin("sandbox", "writes"); !ok || ref.Name != "local.yaml" || ref.Priority != 10 {
		t.Errorf("sandbox.writes provenance = %+v, %v", ref, ok)
	}
	if ref, ok := doc.Origin("name"); !ok || ref.Name != "base.yaml" {
		t.Errorf("name provenance = %+v, %v", ref, ok)
	}
}

// A key present in two layers is answered by the higher one, and the lower one
// is not merged into it: the provenance says "local.yaml" for the whole list.
func TestLoadProvenanceFollowsTheLastWriter(t *testing.T) {
	_, doc := mustLoad(t, base(), override(platformPaths(`
sandbox:
  net:
    mode: allow_list
    allow_hosts: [api.example.com]
`)))
	if ref, ok := doc.Origin("sandbox", "net", "mode"); !ok || ref.Name != "local.yaml" {
		t.Errorf("net.mode provenance = %+v, %v", ref, ok)
	}
	if ref, ok := doc.Origin("files", "roots"); !ok || ref.Name != "base.yaml" {
		t.Errorf("files.roots provenance = %+v, %v", ref, ok)
	}
}

func TestLoadOperatesOnMappings(t *testing.T) {
	cfg, _ := mustLoad(t, base(), override(`
sandbox:
  net:
    mode: allow_list
    allow_hosts: [api.example.com]
`))
	if cfg.Sandbox.Net.Mode != "allow_list" {
		t.Errorf("net.mode = %q", cfg.Sandbox.Net.Mode)
	}
	if len(cfg.Sandbox.Net.AllowHosts) != 1 {
		t.Errorf("net.allow_hosts = %v", cfg.Sandbox.Net.AllowHosts)
	}
}

func TestLoadRejectsDuplicatePriority(t *testing.T) {
	_, _, err := load(t, base(), Layer{Priority: 0, Name: "other.yaml", Source: Bytes([]byte("name: x"))})
	if err == nil {
		t.Fatal("two layers with the same priority were accepted")
	}
	if !strings.Contains(err.Error(), "priority") {
		t.Errorf("error %q does not mention priority", err)
	}
}

// A typo in a permission section must not be silently ignored.
func TestLoadRejectsUnknownKey(t *testing.T) {
	_, _, err := load(t, base(), override("sandbox:\n  writse: [/tmp]\n"))
	if err == nil {
		t.Fatal("unknown key accepted")
	}
	if !strings.Contains(err.Error(), "writse") {
		t.Errorf("error %q does not name the key", err)
	}
}

// Two keys that YAML accepts but the merge would collapse into one value are
// refused: the merge would otherwise take the last one in silence.
func TestLoadRejectsDuplicateKeyInOneLayer(t *testing.T) {
	_, _, err := load(t, base(), override("sandbox:\n  backend: local\n  backend: bwrap\n"))
	if err == nil {
		t.Fatal("a duplicate key inside one layer was accepted")
	}
	if !strings.Contains(err.Error(), "backend") {
		t.Errorf("error %q does not name the key", err)
	}
}

func TestLoadExpandsEnv(t *testing.T) {
	root := pathtest.Abs("expanded")
	t.Setenv("CONFIG_TEST_ROOT", root)
	cfg, _ := mustLoad(t, Layer{Priority: 0, Name: "base.yaml", Source: Bytes([]byte(platformPaths(`
version: 1
sandbox:
  backend: local
  workdir: ${CONFIG_TEST_ROOT}
files:
  roots: ["${CONFIG_TEST_ROOT}"]
`)))})
	if cfg.Files.Roots[0] != root || cfg.Sandbox.WorkDir != root {
		t.Errorf("roots=%v workdir=%q, want the expanded value", cfg.Files.Roots, cfg.Sandbox.WorkDir)
	}
}

// An unset variable with no fallback is an error: an empty value becomes a
// relative path, and the failure would then point at the wrong field.
func TestLoadRejectsUnsetEnv(t *testing.T) {
	_, _, err := load(t, Layer{Priority: 0, Name: "base.yaml", Source: Bytes([]byte(`
version: 1
sandbox:
  backend: local
files:
  roots: ["${CONFIG_TEST_ABSENT}"]
`))})
	if err == nil {
		t.Fatal("unset variable accepted")
	}
	if !strings.Contains(err.Error(), "CONFIG_TEST_ABSENT") {
		t.Errorf("error %q does not name the variable", err)
	}
}

func TestLoadFallback(t *testing.T) {
	cfg, _ := mustLoad(t, Layer{Priority: 0, Name: "base.yaml", Source: Bytes([]byte(platformPaths(`
version: 1
sandbox:
  backend: local
files:
  roots: ["${CONFIG_TEST_ABSENT:-/fallback}"]
`)))})
	if cfg.Files.Roots[0] != pathtest.Abs("fallback") {
		t.Errorf("roots = %v, want the fallback", cfg.Files.Roots)
	}
}

func TestLoadFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "base.yaml")
	body := "version: 1\nsandbox:\n  backend: local\nfiles:\n  roots: [" + t.TempDir() + "]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write layer: %v", err)
	}
	if _, _, err := load(t, Layer{Priority: 0, Name: "base.yaml", Source: File(path)}); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// A commented-out example must not be able to break a working configuration:
// expansion runs on the parsed tree, so comments are already gone.
func TestLoadIgnoresCommentedVariables(t *testing.T) {
	cfg, _ := mustLoad(t, Layer{Priority: 0, Name: "base.yaml", Source: Bytes([]byte(platformPaths(`
version: 1
sandbox:
  backend: local
files:
  roots: ["/work"]
# mcp:
#   servers:
#     - env: { GREETING: "${TOTALLY_UNSET}" }
`)))})
	if cfg.Files.Roots[0] != pathtest.Abs("work") {
		t.Errorf("roots = %v", cfg.Files.Roots)
	}
}

// A "#" inside a quoted value is data, not a comment, and still expands.
func TestLoadExpandsInsideQuotedValues(t *testing.T) {
	t.Setenv("CONFIG_TEST_HOST", "api.example.test")
	cfg, _ := mustLoad(t, Layer{Priority: 0, Name: "base.yaml", Source: Bytes([]byte(platformPaths(`
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
        endpoint: "https://${CONFIG_TEST_HOST}/v1#fragment"
  models:
    - provider: primary
      model: m
`)))})
	var settings map[string]any
	if err := json.Unmarshal(cfg.Inference.Providers[0].Settings, &settings); err != nil {
		t.Fatalf("settings: %v", err)
	}
	if got := settings["endpoint"]; got != "https://api.example.test/v1#fragment" {
		t.Errorf("endpoint = %v", got)
	}
}

// A layer that carries only comments describes no document, and a merge of that
// is not a configuration with defaults: it is empty.
func TestLoadRejectsAnEmptyDocument(t *testing.T) {
	_, _, err := load(t, Layer{Priority: 0, Name: "base.yaml", Source: Bytes([]byte("# nothing here\n"))})
	if err == nil {
		t.Fatal("an empty document was accepted")
	}
	if !strings.Contains(err.Error(), "empty document") {
		t.Errorf("error = %v, want the empty-document sentence", err)
	}
}

func TestLoadRequiresOneLayer(t *testing.T) {
	if _, _, err := load(t); err == nil {
		t.Fatal("an empty stack was accepted")
	}
}

func TestLoadRejectsALayerWithoutSource(t *testing.T) {
	_, _, err := load(t, Layer{Priority: 0, Name: "base.yaml"})
	if err == nil || !strings.Contains(err.Error(), "no source") {
		t.Fatalf("err = %v, want the missing-source sentence", err)
	}
}

// The label is what the caller calls this document, and it is what every error
// starts with: a host that composes two loads must be able to tell which one
// refused.
func TestLoadUsesTheLabelInErrors(t *testing.T) {
	_, _, err := Load[testConfig](context.Background(),
		[]Layer{{Priority: 0, Name: "a.yaml", Source: Bytes([]byte("nope: 1\n"))}},
		Strict[testConfig], WithLabel("runtime"))
	if err == nil || !strings.HasPrefix(err.Error(), "runtime: ") {
		t.Fatalf("err = %v, want the label as the prefix", err)
	}
}

// One layer holds one document. YAML silently stops at the first `---`, so a
// file that holds two would contribute the first and drop the second without a
// word — the refusal is the only way the reader learns.
func TestLoadRejectsMoreThanOneDocumentInALayer(t *testing.T) {
	_, _, err := load(t, Layer{Priority: 0, Name: "a.yaml", Source: Bytes([]byte(
		"version: 1\nname: first\n---\nname: second\n"))})
	if err == nil {
		t.Fatal("a layer holding two documents was accepted")
	}
	if !strings.Contains(err.Error(), "more than one YAML document") {
		t.Errorf("err = %v, want the multi-document sentence", err)
	}
}

// A layer that is only comments contributes nothing, and is not an error: a
// stack refused for that would blame a file nobody meant to be load-bearing.
func TestLoadToleratesACommentOnlyLayer(t *testing.T) {
	cfg, _ := mustLoad(t,
		Layer{Priority: -10, Name: "comments.yaml", Source: Bytes([]byte("# nothing to see\n"))},
		base(),
	)
	if cfg.Name != "forge" {
		t.Errorf("name = %q", cfg.Name)
	}
}

// JSON is a YAML subset, so a .json layer needs no second parser: it merges,
// expands and decodes through the same path.
func TestLoadAcceptsAJSONLayer(t *testing.T) {
	cfg, _ := mustLoad(t, Layer{Priority: 0, Name: "a.json", Source: Bytes([]byte(
		`{"version":1,"name":"json","sandbox":{"backend":"local"},"files":{"roots":[pathtest.Abs("work")]}}`))})
	if cfg.Name != "json" || cfg.Sandbox.Backend != "local" {
		t.Errorf("cfg = %+v", cfg)
	}
}
