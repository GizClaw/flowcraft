package projection

import (
	"testing"

	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// newTestWorkspace returns an isolated local workspace for one test.
func newTestWorkspace(t *testing.T) workspace.Workspace {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

// testShard is the one shard the store's own tests write. They pin the base and
// segment protocol -- chaining, compaction, residency, reclaim -- which lives
// below the shard boundary, so one shard is enough; the routing tests cover what
// happens above it.
const testShard = "c:test"

// testAddress names that shard within a scope.
func testAddress(scope corememory.Scope) Address {
	return Address{Scope: scope, Shard: testShard}
}
