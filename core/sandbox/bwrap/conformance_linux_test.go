//go:build linux && integration_bwrap

package bwrap

import (
	"os/exec"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox/sandboxtest"
)

// TestRunnerContract runs the cross-backend conformance suite inside a real
// bubblewrap namespace: the same assertions local and seatbelt run, so a
// backend-specific break cannot hide behind a hand-written test that only
// covers what this backend happens to do.
func TestRunnerContract(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skipf("bwrap is not installed: %v", err)
	}
	sandboxtest.Run(t, func(t *testing.T, root string) sandboxtest.Harness {
		t.Helper()
		runner, err := New(root)
		if err != nil {
			t.Fatalf("bwrap New: %v", err)
		}
		t.Cleanup(func() { _ = runner.Close() })
		return sandboxtest.Harness{Runner: runner, Root: root}
	})
}
