//go:build darwin

package seatbelt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/engine"
)

// TestOnePermissionPolicyDrivesBothEnforcers is the alignment invariant in
// executable form: one permission.Writes value feeds the in-process handles
// (fsx) and the kernel projection (this backend), and the two must agree.
//
// The failure it guards against is the one the old tree shipped: two boundary
// definitions that could drift, where a widening on one side is invisible
// because nothing compares them.
func TestOnePermissionPolicyDrivesBothEnforcers(t *testing.T) {
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
		t.Fatalf("seatbelt.New: %v", err)
	}
	t.Cleanup(func() { _ = runner.Close() })

	ctx := context.Background()

	// The allowed path: both sides accept it.
	inside := filepath.Join(work, "inside.txt")
	if err := tree.Write(ctx, inside, []byte("x")); err != nil {
		t.Fatalf("fsx write inside the writable set: %v", err)
	}
	res, err := engine.Exec(ctx, runner, "/bin/sh",
		[]string{"-c", `echo x > "$1"`, "sh", inside}, sandbox.ExecOptions{})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("kernel denied a write the policy allows: %v %+v", err, res)
	}

	// Everything else: both sides refuse.
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
}
