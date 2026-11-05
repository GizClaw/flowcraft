//go:build linux

package bwrap

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/engine"
)

// TestOnePermissionPolicyDrivesBothEnforcers is the bwrap half of the alignment
// invariant: one permission.Policy feeds the in-process interpreter (fsx) and
// the kernel projection (this backend), and the two must agree.
//
// It needs a real bubblewrap binary, so it self-skips where bwrap is absent;
// in a Linux CI container it runs for real.
func TestOnePermissionPolicyDrivesBothEnforcers(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}

	root := t.TempDir()
	work := filepath.Join(root, "work")

	policy := permission.Policy{Files: permission.Files{Entries: []permission.Entry{
		{Path: work, Access: permission.Write},
	}}}
	if err := policy.Validate(); err != nil {
		t.Fatalf("policy.Validate: %v", err)
	}

	inproc, err := fsx.NewLocal(root, fsx.WithWritable(policy.Files.Writable()...))
	if err != nil {
		t.Fatalf("fsx.NewLocal: %v", err)
	}
	ctx := context.Background()
	if err := inproc.Write(ctx, "work/inproc.txt", []byte("x"), nil); err != nil {
		t.Fatalf("fsx write inside the allowed path: %v", err)
	}
	if err := inproc.Write(ctx, "root.txt", []byte("x"), nil); !errors.Is(err, fsx.ErrDenied) {
		t.Errorf("fsx write outside the allowed path = %v, want fsx.ErrDenied", err)
	}

	runner, err := New(root, WithReadOnlyRoot(), WithWritablePaths(policy.Files.Writable()...))
	if err != nil {
		t.Fatalf("bwrap.New: %v", err)
	}
	t.Cleanup(func() { _ = runner.Close() })

	write := func(path string) *sandbox.ExecResult {
		t.Helper()
		res, err := engine.Exec(ctx, runner, "/bin/sh",
			[]string{"-c", `echo x > "$1"`, "sh", path}, sandbox.ExecOptions{})
		if err != nil {
			return &sandbox.ExecResult{ExitCode: -1, Stderr: err.Error()}
		}
		return res
	}

	if res := write(filepath.Join(work, "kernel.txt")); res.ExitCode != 0 {
		t.Fatalf("kernel denied a write the policy allows: %s", res.Stderr)
	}

	for _, path := range []string{
		filepath.Join(root, "root.txt"),           // inside the tree, outside the policy
		filepath.Join(t.TempDir(), "outside.txt"), // outside the tree
	} {
		if res := write(path); res.ExitCode == 0 {
			t.Errorf("kernel allowed a write the policy denies: %s", path)
		}
		if err := inproc.Write(ctx, path, []byte("x"), nil); !errors.Is(err, fsx.ErrDenied) {
			t.Errorf("fsx allowed a write the policy denies: %s (%v)", path, err)
		}
	}
}
