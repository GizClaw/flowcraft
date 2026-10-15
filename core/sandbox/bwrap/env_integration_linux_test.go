//go:build linux && integration_bwrap

package bwrap

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

// TestChildEnvReachesTheSandbox is the cross-process env assertion: the
// allow-list is what the sandboxed command actually sees. The runner
// installs the environment on the bwrap command instead of --setenv
// flags, so only a real exec can prove a variable arrives — and that a
// host variable outside the list does not.
func TestChildEnvReachesTheSandbox(t *testing.T) {
	envPath, err := exec.LookPath("env")
	if err != nil {
		t.Skipf("env is not installed: %v", err)
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skipf("bwrap is not installed: %v", err)
	}

	root := t.TempDir()
	runner, err := New(root)
	if err != nil {
		t.Fatalf("bwrap New: %v", err)
	}
	t.Cleanup(func() { _ = runner.Close() })

	res, err := sandbox.Exec(context.Background(), runner, envPath, nil,
		sandbox.ExecOptions{
			WorkDir: root,
			Env: sandbox.EnvPolicy{
				Allow:  []string{"PATH"},
				Inject: map[string]string{"FLOWCRAFT_PROBE": "injected"},
			},
		})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	for _, marker := range namespaceFailures {
		if strings.Contains(res.Stderr, marker) {
			t.Skipf("bwrap cannot create a namespace here: %s", strings.TrimSpace(res.Stderr))
		}
	}
	if res.ExitCode != 0 {
		t.Fatalf("exec: exit %d: %s", res.ExitCode, res.Stderr)
	}

	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			got[name] = value
		}
	}

	want := map[string]string{
		"PATH":            os.Getenv("PATH"),
		"FLOWCRAFT_PROBE": "injected",
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s = %q, want %q (env: %v)", name, got[name], value, res.Stdout)
		}
	}
	// bwrap sets PWD itself, to the sandboxed command's working
	// directory (the policy's WorkDir); the policy never supplies it.
	// Any other value means the host environment leaked through.
	if pwd, ok := got["PWD"]; ok && pwd != root {
		t.Errorf("PWD = %q, want the workdir %q", pwd, root)
	}
	for name, value := range got {
		switch name {
		case "PATH", "FLOWCRAFT_PROBE", "PWD":
		default:
			t.Errorf("a variable outside the allow-list reached the sandbox: %s=%q", name, value)
		}
	}
}
