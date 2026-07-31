//go:build darwin

package seatbelt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

func TestRunner_Enforcement(t *testing.T) {
	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := runner.Enforcement()
	if !got.EnvAllowList || !got.MemoryCap || !got.CPUCap || !got.FilesystemBounds {
		t.Errorf("missing enforcement claims: %+v", got)
	}
	if got.DiskCap {
		t.Error("Seatbelt must not claim disk quotas")
	}
	if len(got.NetModes) != 1 || got.NetModes[0] != sandbox.NetDenyAll {
		t.Errorf("NetModes = %v, want [NetDenyAll]", got.NetModes)
	}
}

func TestRunner_ExecAndEnvironment(t *testing.T) {
	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := runner.Exec(
		context.Background(),
		"/bin/sh",
		[]string{"-c", `printf "%s:%s" "$FLOWCRAFT_TEST" "${HOME-unset}"`},
		sandbox.ExecOptions{
			Env: sandbox.EnvPolicy{
				Allow:  []string{},
				Inject: map[string]string{"FLOWCRAFT_TEST": "ok"},
			},
		},
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 || result.Stdout != "ok:unset" {
		t.Fatalf("result = %+v, want stdout ok:unset", result)
	}
}

func TestRunner_WorkDirEscapeRejected(t *testing.T) {
	root := t.TempDir()
	runner, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Exec(context.Background(), "/bin/pwd", nil, sandbox.ExecOptions{
		WorkDir: t.TempDir(),
	})
	if !errors.Is(err, sandbox.ErrPathTraversal) {
		t.Fatalf("expected ErrPathTraversal, got %v", err)
	}
}

func TestRunner_UnsupportedPolicies(t *testing.T) {
	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, opts := range map[string]sandbox.ExecOptions{
		"disk": {Resources: sandbox.ResourceLimits{DiskBytes: 1}},
		"cpu-without-timeout": {
			Resources: sandbox.ResourceLimits{CPUMillicores: 100},
		},
		"net-allow-list": {
			Net: sandbox.NetPolicy{Mode: sandbox.NetAllowList},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runner.Exec(context.Background(), "/usr/bin/true", nil, opts)
			if !errdefs.IsNotAvailable(err) {
				t.Fatalf("expected NotAvailable, got %v", err)
			}
		})
	}
}

func TestRunner_FilesystemWriteBound(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	runner, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "inside.txt")
	out := filepath.Join(outside, "outside.txt")
	script := `printf inside > "$1"; printf outside > "$2"`
	result, err := runner.Exec(context.Background(), "/bin/sh",
		[]string{"-c", script, "sh", inside, out},
		sandbox.ExecOptions{Timeout: 5 * time.Second},
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("outside write should make the shell fail: %+v", result)
	}
	if data, err := os.ReadFile(inside); err != nil || string(data) != "inside" {
		t.Fatalf("inside write failed: data=%q err=%v", data, err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("outside file should not exist, stat err=%v", err)
	}
	if !strings.Contains(result.Stderr, "Operation not permitted") {
		t.Logf("Seatbelt rejection stderr differed: %q", result.Stderr)
	}
}
