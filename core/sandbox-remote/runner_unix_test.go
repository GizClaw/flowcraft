//go:build unix

package remote_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-local"
)

// newInner builds the backend the server runs commands on: the no-isolation
// runner on unix, the Windows one on Windows. The wire protocol does not care
// which backend answers, which is exactly what these tests check — so they run
// wherever the platform can start a process.
func newInner(tb testing.TB, root string) sandbox.Runner {
	tb.Helper()
	runner := local.New(root)
	tb.Cleanup(func() { _ = runner.Close() })
	return runner
}
