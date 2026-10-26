package graph

import (
	"os"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
)

// TestMain turns the board var-ownership audit on for the whole package: node
// handlers, parallel merges and checkpoint writers all read vars back from the
// board, and the incremental checkpoint path only records writes that go
// through Board. A handler that mutates a var in place would make a delta miss
// the change, so it has to fail here.
func TestMain(m *testing.M) {
	agent.AuditBoardValues(true)
	os.Exit(m.Run())
}
