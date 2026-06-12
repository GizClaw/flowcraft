package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "claw-cli-home-*")
	if err != nil {
		os.Exit(m.Run())
	}
	_ = os.Setenv("CLAW_CONFIG_DIR", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestEnsureClawHomeConfigsSyncsEmbeddedConfigs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAW_CONFIG_DIR", root)

	if err := ensureClawConfigDir(); err != nil {
		t.Fatalf("ensureClawConfigDir: %v", err)
	}

	for _, rel := range []string{
		"configs/raid/chat.yaml",
		"configs/persona/boy_14_Tom.yaml",
		"configs/test/chat/history_window_continuity.yaml",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing synced %s: %v", path, err)
		}
	}
}

func TestEnsureClawHomeConfigsDoesNotOverwriteUserConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAW_CONFIG_DIR", root)
	path := filepath.Join(root, "configs", "raid", "chat.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const custom = "agent:\n  id: custom-chat\n"
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatalf("write custom config: %v", err)
	}

	if err := ensureClawConfigDir(); err != nil {
		t.Fatalf("ensureClawConfigDir: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read custom config: %v", err)
	}
	if string(raw) != custom {
		t.Fatalf("config was overwritten:\n%s", string(raw))
	}
}

func TestReadConfigSourcePrefersClawHomeConfigs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAW_CONFIG_DIR", root)
	path := filepath.Join(root, "configs", "raid", "chat.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const custom = "agent:\n  id: local-chat\n"
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatalf("write local config: %v", err)
	}

	gotPath, raw, err := readConfigSource(templateFS, "chat")
	if err != nil {
		t.Fatalf("readConfigSource: %v", err)
	}
	if gotPath != path {
		t.Fatalf("path = %q, want %q", gotPath, path)
	}
	if string(raw) != custom {
		t.Fatalf("raw = %q, want custom config", string(raw))
	}
}

func TestReadTestSourcePrefersClawHomeConfigs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAW_CONFIG_DIR", root)
	path := filepath.Join(root, "configs", "test", "chat", "history_window_continuity.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const custom = "name: local history test\nraid: chat\nturns:\n  - hi\n"
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatalf("write local test config: %v", err)
	}

	gotPath, raw, err := readTestSource("chat/history_window_continuity")
	if err != nil {
		t.Fatalf("readTestSource: %v", err)
	}
	if gotPath != path {
		t.Fatalf("path = %q, want %q", gotPath, path)
	}
	if string(raw) != custom {
		t.Fatalf("raw = %q, want custom test", string(raw))
	}
}

func TestListConfigsIncludesClawHomeConfigs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAW_CONFIG_DIR", root)
	files := map[string]string{
		"configs/raid/custom_raid.yaml":             "agent:\n  id: custom\n",
		"configs/persona/custom_persona.yaml":       "agent:\n  id: custom-persona\n",
		"configs/test/custom_raid/custom_case.yaml": "name: custom\nraid: custom_raid\nturns:\n  - hi\n",
	}
	for rel, raw := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", rel, err)
		}
	}

	raids, err := listRaids()
	if err != nil {
		t.Fatalf("listRaids: %v", err)
	}
	if !contains(raids, "custom_raid") {
		t.Fatalf("raids = %v, want custom_raid", raids)
	}
	personas, err := listPersonas()
	if err != nil {
		t.Fatalf("listPersonas: %v", err)
	}
	if !contains(personas, "custom_persona") {
		t.Fatalf("personas = %v, want custom_persona", personas)
	}
	tests, err := listTests()
	if err != nil {
		t.Fatalf("listTests: %v", err)
	}
	if !contains(tests, "custom_raid/custom_case") {
		t.Fatalf("tests = %v, want custom_raid/custom_case", tests)
	}
}

func TestExecuteSyncsClawHomeConfigs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAW_CONFIG_DIR", root)

	if err := Execute([]string{"help"}); err != nil {
		t.Fatalf("Execute help: %v", err)
	}

	path := filepath.Join(root, "configs", "raid", "chat.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("missing synced chat config: %v", err)
	}
}
