package retrieval

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/workspace"
)

// newTestWorkspace returns an isolated local workspace for one test.
func newTestWorkspace(t *testing.T) workspace.Workspace {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ws
}
