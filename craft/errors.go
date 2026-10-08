package craft

import "github.com/GizClaw/flowcraft/core/errdefs"

// Sentinel errors returned by the Craft lifecycle.
var (
	// ErrCraftClosed reports an operation on a closed Craft.
	ErrCraftClosed = errdefs.NotAvailablef("craft: closed")
	// ErrNotStarted reports an operation that requires Start first.
	ErrNotStarted = errdefs.NotAvailablef("craft: not started")
	// ErrRuntimeNotFound reports an unknown runtime key.
	ErrRuntimeNotFound = errdefs.NotFoundf("craft: runtime not found")
	// ErrRuntimeExists reports a duplicate runtime key.
	ErrRuntimeExists = errdefs.Conflictf("craft: runtime already exists")
)
