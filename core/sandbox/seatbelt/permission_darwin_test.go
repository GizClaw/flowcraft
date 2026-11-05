//go:build darwin

package seatbelt

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/engine"
)

// TestOnePermissionPolicyDrivesBothEnforcers is the alignment invariant, in
// executable form: a single permission.Policy feeds the in-process interpreter
// (fsx) and the kernel projection (this backend), and the two must agree.
//
// The failure this guards against is the one the old tree shipped: a policy
// checked in one place and projected in another, where a widening on one side
// is invisible because nothing compares them.
func TestOnePermissionPolicyDrivesBothEnforcers(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")

	policy := permission.Policy{Files: permission.Files{Entries: []permission.Entry{
		{Path: work, Access: permission.Write},
	}}}
	if err := policy.Validate(); err != nil {
		t.Fatalf("policy.Validate: %v", err)
	}

	// The in-process interpreter.
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

	// The kernel interpreter, fed the same value.
	runner, err := New(root, WithReadOnlyRoot(), WithWritablePaths(policy.Files.Writable()...))
	if err != nil {
		t.Fatalf("seatbelt.New: %v", err)
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
		filepath.Join(root, "root.txt"),             // inside the tree, outside the policy
		filepath.Join(t.TempDir(), "elsewhere.txt"), // outside the tree
	} {
		if res := write(path); res.ExitCode == 0 {
			t.Errorf("kernel allowed a write the policy denies: %s", path)
		}
		if err := inproc.Write(ctx, path, []byte("x"), nil); !errors.Is(err, fsx.ErrDenied) {
			t.Errorf("fsx allowed a write the policy denies: %s (%v)", path, err)
		}
	}
}
