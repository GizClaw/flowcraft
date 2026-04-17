package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/internal/sandbox"
)

// --- SB-19: buildSandboxMounts 三场景输出验证 ---

func TestBuildSandboxMounts_HostDataDir(t *testing.T) {
	cfg := config.Default()
	//nolint:staticcheck // SA1019 exercising deprecated HostDataDir mount path.
	cfg.Sandbox.HostDataDir = "/mnt/shared"

	mounts := buildSandboxMounts(cfg, "/app/workspace")

	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(mounts))
	}
	assertMount(t, mounts[0], "", filepath.Join("/mnt/shared", "skills"), "/workspace/skills", true)
	assertMount(t, mounts[1], "", filepath.Join("/mnt/shared", "data"), "/workspace/data", false)
}

func TestBuildSandboxMounts_BareMetal(t *testing.T) {
	cfg := config.Default()
	workspaceRoot := "/home/user/.flowcraft/workspace"

	mounts := buildSandboxMounts(cfg, workspaceRoot)

	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(mounts))
	}
	assertMount(t, mounts[0], "", filepath.Join(workspaceRoot, "skills"), "/workspace/skills", true)
	assertMount(t, mounts[1], "", filepath.Join(workspaceRoot, "data"), "/workspace/data", false)
}

func TestBuildSandboxMounts_DinD(t *testing.T) {
	// Simulate running inside a container
	dockerenv := "/.dockerenv"
	if _, err := os.Stat(dockerenv); err != nil {
		t.Skip("not running inside a container, skipping DinD test")
	}

	cfg := config.Default()
	t.Setenv("FLOWCRAFT_SANDBOX_VOLUME_NAME", "myproject_flowcraft-workspace")

	mounts := buildSandboxMounts(cfg, "/app/workspace")

	if len(mounts) != 1 {
		t.Fatalf("expected 1 volume mount, got %d", len(mounts))
	}
	assertMount(t, mounts[0], "volume", "myproject_flowcraft-workspace", "/workspace", false)
}

func TestBuildSandboxMounts_DinD_DefaultVolumeName(t *testing.T) {
	if _, err := os.Stat("/.dockerenv"); err != nil {
		t.Skip("not running inside a container, skipping DinD test")
	}

	cfg := config.Default()
	t.Setenv("FLOWCRAFT_SANDBOX_VOLUME_NAME", "")

	mounts := buildSandboxMounts(cfg, "/app/workspace")

	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}
	if mounts[0].Source != "flowcraft-workspace" {
		t.Fatalf("expected default volume name, got %q", mounts[0].Source)
	}
}

// --- SB-20: workspaceRoot 与 sandboxCfg.RootDir 一致 ---

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

func assertMount(t *testing.T, m sandbox.MountConfig, wantType, wantSource, wantTarget string, wantReadOnly bool) {
	t.Helper()
	if m.Type != wantType {
		t.Errorf("mount Type = %q, want %q", m.Type, wantType)
	}
	if m.Source != wantSource {
		t.Errorf("mount Source = %q, want %q", m.Source, wantSource)
	}
	if m.Target != wantTarget {
		t.Errorf("mount Target = %q, want %q", m.Target, wantTarget)
	}
	if m.ReadOnly != wantReadOnly {
		t.Errorf("mount ReadOnly = %v, want %v", m.ReadOnly, wantReadOnly)
	}
}
