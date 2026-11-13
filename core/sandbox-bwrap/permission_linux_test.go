//go:build linux

package bwrap

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/engine"
)

// TestOnePermissionPolicyDrivesBothEnforcers is the bwrap half of the alignment
// invariant: one permission.Writes value feeds the in-process handles (fsx) and
// the kernel projection (this backend), and the two must agree.
//
// It needs a real bubblewrap binary, so it self-skips where bwrap is absent; in
// a Linux CI container it runs for real.
func TestOnePermissionPolicyDrivesBothEnforcers(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}

	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}

	writes := permission.Writes{Paths: []string{work}}

	tree, err := fsx.Open(fsx.WithWritable(writes.Writable()...))
	if err != nil {
		t.Fatalf("fsx.Open: %v", err)
	}
	t.Cleanup(func() { _ = tree.Close() })

	runner, err := New(root, WithReadOnlyRoot(), WithWritablePaths(writes.Writable()...))
	if err != nil {
		t.Fatalf("bwrap.New: %v", err)
	}
	t.Cleanup(func() { _ = runner.Close() })

	ctx := context.Background()

	inside := filepath.Join(work, "inside.txt")
	if err := tree.Write(ctx, inside, []byte("x")); err != nil {
		t.Fatalf("fsx write inside the writable set: %v", err)
	}
	res, err := engine.Exec(ctx, runner, "/bin/sh",
		[]string{"-c", `echo x > "$1"`, "sh", inside}, sandbox.ExecOptions{})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("kernel denied a write the policy allows: %v %+v", err, res)
	}

	// Not /tmp: bwrap mounts a private tmpfs there, so a write under /tmp
	// succeeds inside the sandbox and never reaches the host — allowed by
	// design, not a policy violation, and therefore not a useful negative case.
	outside := []string{
		filepath.Join(root, "root.txt"),      // inside the runner root, outside the policy
		"/etc/fsx-denied-by-policy-test.txt", // outside the runner root entirely
	}
	for _, path := range outside {
		if err := tree.Write(ctx, path, []byte("x")); !errors.Is(err, fsx.ErrDenied) {
			t.Errorf("fsx write to %s = %v, want fsx.ErrDenied", path, err)
		}
		res, err := engine.Exec(ctx, runner, "/bin/sh",
			[]string{"-c", `echo x > "$1"`, "sh", path}, sandbox.ExecOptions{})
		if err == nil && res.ExitCode == 0 {
			t.Errorf("kernel allowed a write the same policy denies: %s", path)
		}
	}
}
