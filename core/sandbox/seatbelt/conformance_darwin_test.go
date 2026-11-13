//go:build darwin

package seatbelt

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox/sandboxtest"
)

// TestRunnerContract runs the cross-backend conformance suite against a real
// SBPL profile. The suite is the contract, so this file deliberately contains
// nothing but the construction: everything it asserts lives in sandboxtest,
// where the other backends run the same assertions.
func TestRunnerContract(t *testing.T) {
	sandboxtest.Run(t, func(t *testing.T, root string) sandboxtest.Harness {
		t.Helper()
		runner, err := New(root)
		if err != nil {
			t.Fatalf("seatbelt New: %v", err)
		}
		t.Cleanup(func() { _ = runner.Close() })
		return sandboxtest.Harness{Runner: runner, Root: root}
	})
}
