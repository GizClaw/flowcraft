//go:build windows

package windows

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// requireConfinement probes whether the host can spawn write-confined
// processes. Hosts without the CreateProcessAsUser privileges
// (SE_INCREASE_QUOTA_NAME) fail closed with NotAvailable; the
// confinement behaviors cannot be exercised there, so the test skips.
func requireConfinement(t *testing.T, r *Runner) {
	t.Helper()
	// Probe with WriteReadOnly so the probe itself does not label the
	// runner root (which would mask write-denial assertions in the
	// caller's test).
	_, err := r.Exec(context.Background(), "cmd", []string{"/c", "ver"},
		sandbox.ExecOptions{Write: sandbox.WriteReadOnly})
	if err == nil {
		return
	}
	if errdefs.IsNotAvailable(err) {
		t.Skipf("host cannot spawn write-confined processes: %v", err)
	}
	t.Fatalf("confinement probe: %v", err)
}

func TestWithLowTempEnvReplacesTemp(t *testing.T) {
	got := withLowTempEnv([]string{"PATH=x", "TEMP=old", "TMP=old2", "HOME=h"}, `C:\low`)
	seen := map[string]string{}
	for _, kv := range got {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("bad entry %q", kv)
		}
		seen[k] = v
	}
	want := map[string]string{
		"PATH":   "x",
		"HOME":   "h",
		"TEMP":   `C:\low`,
		"TMP":    `C:\low`,
		"TMPDIR": `C:\low`,
	}
	if len(seen) != len(want) {
		t.Fatalf("keys = %v, want %v", seen, want)
	}
	for k, v := range want {
		if seen[k] != v {
			t.Fatalf("%s = %q, want %q", k, seen[k], v)
		}
	}
}

func TestWriteConfinementCapabilities(t *testing.T) {
	confined, err := New(t.TempDir(), WithWriteConfinement())
	if err != nil {
		t.Fatalf("New(confined): %v", err)
	}
	defer func() { _ = confined.Close() }()
	caps := confined.Capabilities()
	if !caps.Policy.FilesystemBounds {
		t.Error("confined runner FilesystemBounds = false, want true")
	}
	if len(caps.Policy.WriteModes) != 2 {
		t.Errorf("WriteModes = %v, want WriteWorkspace + WriteReadOnly", caps.Policy.WriteModes)
	}

	plain, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New(plain): %v", err)
	}
	defer func() { _ = plain.Close() }()
	if plain.Capabilities().Policy.FilesystemBounds {
		t.Error("plain runner FilesystemBounds = true, want false")
	}
	if len(plain.Capabilities().Policy.WriteModes) != 0 {
		t.Errorf("plain runner WriteModes = %v, want none", plain.Capabilities().Policy.WriteModes)
	}
}

func TestWriteConfinementAllowsWorkspaceWrite(t *testing.T) {
	root := t.TempDir()
	r, err := New(root, WithWriteConfinement())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = r.Close() }()
	requireConfinement(t, r)

	res, err := r.Exec(context.Background(), "cmd",
		[]string{"/c", "echo", "hi", ">", "out.txt"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr = %q", res.ExitCode, res.Stderr)
	}
	data, err := os.ReadFile(filepath.Join(root, "out.txt"))
	if err != nil {
		t.Fatalf("read out.txt: %v", err)
	}
	if !strings.Contains(string(data), "hi") {
		t.Fatalf("out.txt = %q, want hi", data)
	}
}

func TestWriteConfinementReadOnlyRejectsWrite(t *testing.T) {
	root := t.TempDir()
	r, err := New(root, WithWriteConfinement())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = r.Close() }()
	requireConfinement(t, r)

	res, err := r.Exec(context.Background(), "cmd",
		[]string{"/c", "echo", "hi", ">", "out.txt"},
		sandbox.ExecOptions{Write: sandbox.WriteReadOnly})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("write to read-only root succeeded")
	}
	if _, err := os.Stat(filepath.Join(root, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("out.txt exists after denied write: %v", err)
	}
}

func TestWriteConfinementBlocksOutsideWrite(t *testing.T) {
	root := t.TempDir()
	r, err := New(root, WithWriteConfinement())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = r.Close() }()
	requireConfinement(t, r)

	outside, err := os.MkdirTemp("", "flowcraft-out-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer func() { _ = os.RemoveAll(outside) }()
	target := filepath.Join(outside, "x.txt")

	res, err := r.Exec(context.Background(), "cmd",
		[]string{"/c", "echo", "hi", ">", target}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("write outside the workspace succeeded")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target exists after denied write: %v", err)
	}
}

func TestWriteConfinementTempOverride(t *testing.T) {
	r, err := New(t.TempDir(), WithWriteConfinement())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = r.Close() }()
	requireConfinement(t, r)

	res, err := r.Exec(context.Background(), "cmd",
		[]string{"/c", "echo", "%TEMP%"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr = %q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "flowcraft-low-") {
		t.Fatalf("TEMP = %q, want flowcraft-low-*", res.Stdout)
	}
}
