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
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/engine"
)

// TestMain lets the test binary double as the fsx helper: fsx writes by
// re-executing this process with a magic argument and speaking JSON to it.
func TestMain(m *testing.M) {
	fsx.MaybeRunHelper()
	os.Exit(m.Run())
}

// TestOnePermissionPolicyDrivesBothEnforcers is the bwrap half of the
// alignment invariant: one writable set, projected once, and the fsx helper
// (which does no policy check of its own) must agree with a command started
// directly through the same runner.
//
// It needs a real bubblewrap binary, so it self-skips where bwrap is absent;
// in a Linux CI container it runs for real.
func TestOnePermissionPolicyDrivesBothEnforcers(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}

	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}

	runner, err := New(root, WithReadOnlyRoot(), WithWritablePaths(work))
	if err != nil {
		t.Fatalf("bwrap.New: %v", err)
	}
	t.Cleanup(func() { _ = runner.Close() })

	// bwrap mounts a private tmpfs over /tmp, and the go test binary lives
	// under /tmp: inside the sandbox it does not exist. Stage a copy under the
	// runner root, which is bound, and point the helper at it.
	helper := filepath.Join(root, "fsx-helper")
	if err := copyExecutable(helper); err != nil {
		t.Fatalf("stage helper: %v", err)
	}

	tree, err := fsx.Open(fsx.WithWritable(work), fsx.WithSandbox(runner), fsx.WithHelper(helper))
	if err != nil {
		t.Fatalf("fsx.Open: %v", err)
	}

	ctx := context.Background()
	if err := tree.Write(ctx, filepath.Join(work, "fsx.txt"), []byte("x")); err != nil {
		t.Fatalf("fsx write inside the writable set: %v", err)
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

	// Reads are unrestricted: the same tree lists a directory the policy does
	// not mention, which is the whole point of not projecting read restrictions.
	if _, err := tree.List(ctx, root); err != nil {
		t.Errorf("List(%s) = %v, want nil (reads are unrestricted)", root, err)
	}
}

// copyExecutable copies this process to dst with its executable bit intact.
func copyExecutable(dst string) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}
