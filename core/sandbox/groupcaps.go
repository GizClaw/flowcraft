package sandbox

import (
	"context"
	"time"
)

// groupCapsAvailable reports whether group-level resource caps (memory,
// cpu-time) can be enforced here. The contract layer ships the
// conservative answer: the sampling watcher lives with the backend
// implementations, and a limit that silently never fires is worse than a
// limit that is refused.
//
// Backends that link the watcher report their own capability through
// Capabilities.Policy; ValidateExecPolicy keeps refusing actionable caps
// until a runner that can enforce them is wired in.
func groupCapsAvailable() bool { return false }

// GroupCapsWatcher is the contract-layer placeholder for the group
// sampler. The enforcing implementation ships with the backends; this
// stub keeps the exported surface stable for Runner authors.
type GroupCapsWatcher struct{}

// StartGroupCapsWatcher returns nil in the contract layer. Runner.Exec
// rejects actionable caps before reaching this function.
func StartGroupCapsWatcher(_ context.Context, _ int, _ ResourceLimits, _ time.Duration) *GroupCapsWatcher {
	return nil
}

// Stop is nil-safe and a no-op in the contract layer.
func (*GroupCapsWatcher) Stop() {}

// Unenforceable always reports nil here: no sampler runs, so it can never
// break mid-run.
func (*GroupCapsWatcher) Unenforceable() error { return nil }

// Exceeded always reports no cap here.
func (*GroupCapsWatcher) Exceeded() string { return "" }
