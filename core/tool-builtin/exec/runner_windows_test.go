//go:build windows

package exec

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-windows"
)

// newRunnerIn builds the backend these tests drive, rooted at root: the
// no-isolation runner on unix, the Windows one on Windows. The tools do not know
// which, and neither should their tests.
func newRunnerIn(t *testing.T, root string) sandbox.Runner {
	t.Helper()
	runner, err := windows.New(root)
	if err != nil {
		t.Fatalf("windows.New: %v", err)
	}
	t.Cleanup(func() { _ = runner.Close() })
	return runner
}
