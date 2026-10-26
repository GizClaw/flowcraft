package agent

import (
	"os"
	"testing"
)

// TestMain turns the board var-ownership audit on for the whole package: the
// board's write record only sees writes that go through Board, so a test that
// mutates a value it read back must fail here rather than produce an
// incremental checkpoint that silently drops that change.
func TestMain(m *testing.M) {
	AuditBoardValues(true)
	os.Exit(m.Run())
}
