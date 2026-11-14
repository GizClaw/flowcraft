//go:build unix

package exec

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-local"
)

// newRunnerIn builds the backend these tests drive, rooted at root: the
// no-isolation runner on unix, the Windows one on Windows. The tools do not know
// which, and neither should their tests.
func newRunnerIn(t *testing.T, root string) sandbox.Runner {
	t.Helper()
	runner := local.New(root)
	t.Cleanup(func() { _ = runner.Close() })
	return runner
}
