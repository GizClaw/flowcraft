//go:build windows

package remote_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-windows"
)

// newInner builds the backend the server runs commands on: the no-isolation
// runner on unix, the Windows one on Windows.
func newInner(tb testing.TB, root string) sandbox.Runner {
	tb.Helper()
	runner, err := windows.New(root)
	if err != nil {
		tb.Fatalf("windows.New: %v", err)
	}
	tb.Cleanup(func() { _ = runner.Close() })
	return runner
}
