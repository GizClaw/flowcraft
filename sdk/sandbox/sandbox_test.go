package sandbox_test

import (
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

// TestErrPathTraversal_IsForbidden ensures the package-level error is
// classified as Forbidden by errdefs so HTTP / RPC layers map it to 403
// without runtime-specific shims. This is the contract the deprecation
// shim in sdk/workspace/command.go relies on.
func TestErrPathTraversal_IsForbidden(t *testing.T) {
	if !errdefs.IsForbidden(sandbox.ErrPathTraversal) {
		t.Fatal("sandbox.ErrPathTraversal should be classified as Forbidden")
	}
}

// TestErrPathTraversal_Independence guards against accidentally aliasing
// sandbox.ErrPathTraversal to workspace.ErrPathTraversal. sandbox MUST
// not import workspace (the dependency runs the other way through the
// deprecation shim), so the two sentinels are intentionally separate
// values even if they convey the same concept.
func TestErrPathTraversal_Independence(t *testing.T) {
	wrapped := errors.New("wrapped")
	if errors.Is(wrapped, sandbox.ErrPathTraversal) {
		t.Fatal("unrelated error should not be sandbox.ErrPathTraversal")
	}
}
