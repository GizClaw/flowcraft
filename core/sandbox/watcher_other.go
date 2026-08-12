//go:build !unix

package sandbox

import (
	"context"
	"os/exec"
	"time"
)

// groupCapsAvailable is false on non-unix platforms: the sampling
// watcher relies on ps(1) process-group accounting, so resource caps
// must be rejected with errdefs.NotAvailable rather than silently
// skipped.
func groupCapsAvailable() bool { return false }

// GroupCapsWatcher is the non-unix stub of the group sampler.
type GroupCapsWatcher struct{}

// StartGroupCapsWatcher returns nil outside unix. Runner.Exec rejects
// actionable caps before reaching this function.
func StartGroupCapsWatcher(_ context.Context, _ int, _ ResourceLimits, _ time.Duration) *GroupCapsWatcher {
	return nil
}

// Stop is nil-safe and a no-op outside unix.
func (*GroupCapsWatcher) Stop() {}

// Unenforceable always reports nil outside unix: no sampler runs, so
// it can never break mid-run.
func (*GroupCapsWatcher) Unenforceable() error { return nil }

// Exceeded always reports no cap outside unix.
func (*GroupCapsWatcher) Exceeded() string { return "" }
