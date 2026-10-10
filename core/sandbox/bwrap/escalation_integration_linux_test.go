//go:build linux && integration_bwrap

package bwrap

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

// namespaceFailures are the messages bwrap itself leaves when the
// kernel refuses it a namespace: the command never ran, so there is no
// refusal to recognize and the test skips instead of blaming the
// detector.
var namespaceFailures = []string{
	"setting up uid map",
	"setting up gid map",
	"No permissions to create new namespace",
	"Failed to unshare",
}

// TestDeniedRecognizesRealBwrapRefusal pins the refusal spelling
// [sandbox.Denied] matches for this backend to what a real namespace
// writes: a per-call WriteReadOnly leaves the root on a read-only bind,
// and the write that reaches it has to come back as EROFS.
func TestDeniedRecognizesRealBwrapRefusal(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skipf("bwrap is not installed: %v", err)
	}

	root := t.TempDir()
	runner, err := New(root)
	if err != nil {
		t.Fatalf("bwrap New: %v", err)
	}
	t.Cleanup(func() { _ = runner.Close() })

	res, err := sandbox.Exec(context.Background(), runner, "/bin/sh",
		[]string{"-c", "echo hi > " + filepath.Join(root, "denied.txt")},
		sandbox.ExecOptions{WorkDir: root, Write: sandbox.WriteReadOnly})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	for _, marker := range namespaceFailures {
		if strings.Contains(res.Stderr, marker) {
			t.Skipf("bwrap cannot create a namespace here: %s", strings.TrimSpace(res.Stderr))
		}
	}
	if res.ExitCode == 0 {
		t.Fatalf("the write to a read-only root succeeded: %+v", res)
	}
	reason, denied := sandbox.Denied(res)
	if !denied {
		t.Fatalf("Denied missed a real refusal: exit %d, stderr %q, stdout %q",
			res.ExitCode, res.Stderr, res.Stdout)
	}
	if detail := sandbox.DenialExcerpt(res); detail == "" {
		t.Fatalf("DenialExcerpt found no line to show for %q", res.Stderr)
	}
	t.Logf("denial recognized: %s", reason)
}
