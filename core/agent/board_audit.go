package agent

import "sync/atomic"

// boardValueAudit makes [Board.Var] and [Board.Vars] return deep copies
// instead of the stored values.
//
// It exists to police the [Board] ownership contract in tests: a handler that
// mutates a value it read from the board, rather than calling [Board.SetVar],
// bypasses the board's write record — so [Board.Since] would not see the
// change and an incremental checkpoint would silently persist stale state.
// With the audit on, such a handler mutates a copy, its own assertions fail,
// and the violation surfaces where it was introduced instead of as a wrong
// resume months later.
//
// It is a process-wide debug switch: production paths must leave it off,
// because copying on every read turns the hot var reads (node config
// resolution, edge conditions, script `board.getVar`) into allocations.
var boardValueAudit atomic.Bool

// AuditBoardValues turns the var-ownership audit on or off for the process.
// It is meant for tests: call it from a TestMain to make any place in this
// module that mutates a board var in place fail, instead of relying on review.
func AuditBoardValues(enabled bool) {
	boardValueAudit.Store(enabled)
}

// BoardValueAuditEnabled reports whether the var-ownership audit is on.
func BoardValueAuditEnabled() bool {
	return boardValueAudit.Load()
}
