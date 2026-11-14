//go:build windows

package windows_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox-windows"
	"github.com/GizClaw/flowcraft/core/sandbox/sandboxtest"
)

// TestRunnerContract runs the cross-backend conformance suite against the
// Windows backend. Every backend runs this same suite from its own test file, so
// a Windows-only behaviour difference shows up as a failing contract case rather
// than as a surprise in a deployment; the suite renders its scripts for the
// platform through its own Cmd profile.
func TestRunnerContract(t *testing.T) {
	sandboxtest.Run(t, func(t *testing.T, root string) sandboxtest.Harness {
		t.Helper()
		runner, err := windows.New(root)
		if err != nil {
			t.Fatalf("windows.New: %v", err)
		}
		t.Cleanup(func() { _ = runner.Close() })
		return sandboxtest.Harness{Runner: runner, Root: root}
	})
}
