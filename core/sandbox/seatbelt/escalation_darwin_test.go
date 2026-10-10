//go:build darwin

package seatbelt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

// TestDeniedRecognizesRealSeatbeltRefusal pins the refusal spelling
// [sandbox.Denied] matches to what this platform actually writes. The
// marker table is only as good as the stream it recognizes, and a
// scripted fixture cannot prove it: here a real profile denies a real
// write, and the detector has to see it.
func TestDeniedRecognizesRealSeatbeltRefusal(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skipf("sandbox-exec is not installed: %v", err)
	}

	root := t.TempDir()
	runner, err := New(root)
	if err != nil {
		t.Fatalf("seatbelt New: %v", err)
	}
	t.Cleanup(func() { _ = runner.Close() })

	// The profile denies writes everywhere but the runner root, the
	// explicit writable paths and /dev/null, so this target is refused
	// by the write confinement itself, not by a missing parent.
	outside := t.TempDir()
	target := filepath.Join(outside, "denied.txt")

	res, err := sandbox.Exec(context.Background(), runner, "/bin/sh",
		[]string{"-c", "echo hi > " + target},
		sandbox.ExecOptions{WorkDir: root})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("the write outside the root succeeded: %+v", res)
	}
	reason, denied := sandbox.Denied(res)
	if !denied {
		t.Fatalf("Denied missed a real refusal: exit %d, stderr %q, stdout %q",
			res.ExitCode, res.Stderr, res.Stdout)
	}
	if detail := sandbox.DenialExcerpt(res); detail == "" {
		t.Fatalf("DenialExcerpt found no line to show for %q", res.Stderr)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("the denied write left %s behind (stat err: %v): the profile did not confine it",
			target, err)
	}
	t.Logf("denial recognized: %s", reason)
}
