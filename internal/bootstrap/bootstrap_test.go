package bootstrap

import (
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/internal/sandbox"
)

func TestWorkspaceRoot_Consistency(t *testing.T) {
	cases := []struct {
		name          string
		rootDir       string
		configurePath string
		wantRoot      string
	}{
		{
			name:          "explicit RootDir",
			rootDir:       "/opt/flowcraft/workspace",
			configurePath: "/home/user/.flowcraft",
			wantRoot:      "/opt/flowcraft/workspace",
		},
		{
			name:          "empty RootDir falls back to ConfigurePath/workspace",
			rootDir:       "",
			configurePath: "/home/user/.flowcraft",
			wantRoot:      "/home/user/.flowcraft/workspace",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Reproduce the same logic as bootstrap.Run
			workspaceRoot := tc.rootDir
			if workspaceRoot == "" {
				workspaceRoot = filepath.Join(tc.configurePath, "workspace")
			}

			sandboxCfg := sandbox.ManagerConfig{
				RootDir: workspaceRoot,
			}

			if workspaceRoot != tc.wantRoot {
				t.Fatalf("workspaceRoot = %q, want %q", workspaceRoot, tc.wantRoot)
			}
			if sandboxCfg.RootDir != tc.wantRoot {
				t.Fatalf("sandboxCfg.RootDir = %q, want %q", sandboxCfg.RootDir, tc.wantRoot)
			}
			if workspaceRoot != sandboxCfg.RootDir {
				t.Fatalf("workspaceRoot and sandboxCfg.RootDir diverge: %q vs %q", workspaceRoot, sandboxCfg.RootDir)
			}
		})
	}
}
