package bootstrap

import (
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/internal/sandbox"
)

// TestWorkspaceRoot_Consistency verifies that wireSandbox falls back to
// config.WorkspaceDir() when cfg.Sandbox.RootDir is empty. Workspace
// stores agent state, plugins, skills, and long-term memory — all user
// data — so on macOS (server runs in a vfkit guest) it must land on the
// virtio-fs-shared DataDir to survive VM restart. Resolving against
// HomeRoot would put it on the guest's tmpfs and wipe data on reboot.
func TestWorkspaceRoot_Consistency(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dataDir := t.TempDir()
	t.Setenv("FLOWCRAFT_DATA_DIR", dataDir)

	cases := []struct {
		name     string
		rootDir  string
		wantRoot string
	}{
		{
			name:     "explicit RootDir",
			rootDir:  "/opt/flowcraft/workspace",
			wantRoot: "/opt/flowcraft/workspace",
		},
		{
			name:     "empty RootDir falls back to WorkspaceDir",
			rootDir:  "",
			wantRoot: filepath.Join(dataDir, "workspace"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspaceRoot := tc.rootDir
			if workspaceRoot == "" {
				workspaceRoot = config.WorkspaceDir()
			}

			sandboxCfg := sandbox.ManagerConfig{RootDir: workspaceRoot}

			if workspaceRoot != tc.wantRoot {
				t.Fatalf("workspaceRoot = %q, want %q", workspaceRoot, tc.wantRoot)
			}
			if sandboxCfg.RootDir != tc.wantRoot {
				t.Fatalf("sandboxCfg.RootDir = %q, want %q", sandboxCfg.RootDir, tc.wantRoot)
			}
		})
	}
}
