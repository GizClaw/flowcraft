package local_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/sandboxtest"
	"github.com/GizClaw/flowcraft/sandbox/local"
)

// mustNew builds the backend this package tests: no confinement at all, which is the local
// backend's whole surface. A close is registered here so every case releases what it started.
func mustNew(t testing.TB, opts ...local.Option) *local.Runner {
	t.Helper()
	runner, err := local.New(sandbox.Confine{}, opts...)
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	t.Cleanup(func() { _ = runner.Close() })
	return runner
}

// TestRunnerContract runs the cross-backend conformance suite against the no-isolation backend.
// Every other backend runs the same suite from its own test file, so a failure points at the backend
// that broke the contract rather than at the suite.
func TestRunnerContract(t *testing.T) {
	sandboxtest.Run(t, func(t *testing.T, root string) sandboxtest.Harness {
		t.Helper()
		return sandboxtest.Harness{Runner: mustNew(t), Root: root}
	})
}
