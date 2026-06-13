package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCreateOrOpenTUIWorkspaceFromRaidCreatesHashedWorkspace(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAW_CONFIG_DIR", root)
	if err := ensureClawConfigDir(); err != nil {
		t.Fatalf("ensureClawConfigDir: %v", err)
	}

	workspace, err := createNewTUIWorkspaceFromRaid("chat")
	if err != nil {
		t.Fatalf("createOrOpenTUIWorkspaceFromRaid: %v", err)
	}
	wantRoot := filepath.Join(root, "workspaces")
	if filepath.Dir(workspace.Path) != wantRoot {
		t.Fatalf("workspace path = %q, want under %q", workspace.Path, wantRoot)
	}
	if len(filepath.Base(workspace.Path)) != 16 {
		t.Fatalf("workspace id = %q, want 16-char hash", filepath.Base(workspace.Path))
	}
	if _, err := os.Stat(filepath.Join(workspace.Path, configFileName)); err != nil {
		t.Fatalf("missing workspace config: %v", err)
	}
	meta, err := readTUIWorkspaceMeta(workspace.Path)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if meta.Kind != "raid" || meta.ConfigName != "chat" || meta.LastOpenedAt == "" {
		t.Fatalf("metadata = %+v, want raid chat with last_opened", meta)
	}
}

func TestCreateNewTUIWorkspaceFromRaidResetsExistingWorkspace(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAW_CONFIG_DIR", root)
	if err := ensureClawConfigDir(); err != nil {
		t.Fatalf("ensureClawConfigDir: %v", err)
	}
	workspace, err := createNewTUIWorkspaceFromRaid("chat")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	configPath := filepath.Join(workspace.Path, configFileName)
	const custom = "agent:\n  id: edited\n"
	if err := os.WriteFile(configPath, []byte(custom), 0o644); err != nil {
		t.Fatalf("edit config: %v", err)
	}
	progressPath := filepath.Join(workspace.Path, "state", "contexts", "old.json")
	if err := os.MkdirAll(filepath.Dir(progressPath), 0o755); err != nil {
		t.Fatalf("create progress dir: %v", err)
	}
	if err := os.WriteFile(progressPath, []byte("old progress"), 0o644); err != nil {
		t.Fatalf("write progress: %v", err)
	}

	resetWorkspace, err := createNewTUIWorkspaceFromRaid("chat")
	if err != nil {
		t.Fatalf("reset workspace: %v", err)
	}
	if resetWorkspace.Path != workspace.Path {
		t.Fatalf("reset workspace path = %q, want %q", resetWorkspace.Path, workspace.Path)
	}
	if _, err := os.Stat(progressPath); !os.IsNotExist(err) {
		t.Fatalf("progress path still exists or stat failed with non-not-exist: %v", err)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(raw) == custom {
		t.Fatal("config was not reset")
	}
}

func TestCreateOrOpenTUIWorkspaceFromRaidDoesNotOverwriteExistingConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAW_CONFIG_DIR", root)
	if err := ensureClawConfigDir(); err != nil {
		t.Fatalf("ensureClawConfigDir: %v", err)
	}
	workspace, err := createOrOpenTUIWorkspaceFromRaid("chat")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	configPath := filepath.Join(workspace.Path, configFileName)
	const custom = "agent:\n  id: edited\n"
	if err := os.WriteFile(configPath, []byte(custom), 0o644); err != nil {
		t.Fatalf("edit config: %v", err)
	}
	if _, err := createOrOpenTUIWorkspaceFromRaid("chat"); err != nil {
		t.Fatalf("reopen workspace: %v", err)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(raw) != custom {
		t.Fatalf("config overwritten:\n%s", string(raw))
	}
}

func TestListTUIWorkspaceOptionsReadsMetadata(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAW_CONFIG_DIR", root)
	workspace, err := createOrOpenTUIWorkspaceFromRaid("chat")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	options, err := listTUIWorkspaceOptions()
	if err != nil {
		t.Fatalf("listTUIWorkspaceOptions: %v", err)
	}
	if len(options) != 1 {
		t.Fatalf("options = %d, want 1: %+v", len(options), options)
	}
	if options[0].Path != workspace.Path || options[0].ConfigName != "chat" {
		t.Fatalf("option = %+v, want chat workspace %s", options[0], workspace.Path)
	}
}
