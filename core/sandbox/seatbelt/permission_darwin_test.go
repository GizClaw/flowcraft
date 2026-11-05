//go:build darwin

package seatbelt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/engine"
)

// TestMain lets the test binary double as the fsx helper: fsx writes by
// re-executing this process with a magic argument and speaking JSON to it.
func TestMain(m *testing.M) {
	fsx.MaybeRunHelper()
	os.Exit(m.Run())
}

// TestOnePermissionPolicyDrivesBothEnforcers is the alignment invariant: one
// writable set, projected once, and both entry points must agree — the fsx
// helper (which does no policy check of its own) and a command started
// directly through the same runner.
func TestOnePermissionPolicyDrivesBothEnforcers(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}

	runner, err := New(root, WithReadOnlyRoot(), WithWritablePaths(work))
	if err != nil {
		t.Fatalf("seatbelt.New: %v", err)
	}
	t.Cleanup(func() { _ = runner.Close() })

	tree, err := fsx.Open(fsx.WithWritable(work), fsx.WithSandbox(runner))
	if err != nil {
		t.Fatalf("fsx.Open: %v", err)
	}

	ctx := context.Background()
	if err := tree.Write(ctx, filepath.Join(work, "fsx.txt"), []byte("x")); err != nil {
		t.Fatalf("fsx write inside the writable set: %v", err)
	}

	outside := []string{
		filepath.Join(root, "root.txt"),             // inside the runner root, outside the policy
		filepath.Join(t.TempDir(), "elsewhere.txt"), // outside the runner root entirely
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

	// The helper must not be able to widen: reads stay unrestricted, so a read
	// outside the writable set still works.
	if _, err := tree.Read(ctx, filepath.Join(root, "")); err == nil {
		t.Log("root is listable, as expected for unrestricted reads")
	}
}
