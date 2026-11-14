package local_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox-local"
	"github.com/GizClaw/flowcraft/core/sandbox/engine"
	"github.com/GizClaw/flowcraft/core/sandbox/sandboxtest"
)

// TestRunnerContract runs the cross-backend conformance suite against the
// no-isolation backend. Every other backend runs the same suite from its own
// test file, so a failure points at the backend that broke the contract rather
// than at the suite.
func TestRunnerContract(t *testing.T) {
	sandboxtest.Run(t, func(t *testing.T, root string) sandboxtest.Harness {
		t.Helper()
		runner := local.New(root)
		t.Cleanup(func() { _ = runner.Close() })
		return sandboxtest.Harness{Runner: runner, Root: root}
	})
}

// TestRunnerContractWithDecorators covers the other half of "the suite is the
// contract": every decorator forwards start, read, write, list and close, so a
// composed runner satisfies the same session semantics as the bare backend.
//
// OwnsResources is set because engine.WithDefaults deliberately replaces
// ExecOptions.Resources instead of merging it — the daemon owns the budget — so
// a caller's cap never reaches the backend through the chain.
func TestRunnerContractWithDecorators(t *testing.T) {
	sandboxtest.Run(t, func(t *testing.T, root string) sandboxtest.Harness {
		t.Helper()
		runner := engine.ComposeLocal(local.New(root), engine.LocalPolicy{})
		t.Cleanup(func() { _ = runner.Close() })
		return sandboxtest.Harness{Runner: runner, Root: root, OwnsResources: true}
	})
}
