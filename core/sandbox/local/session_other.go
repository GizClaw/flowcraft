//go:build !unix

package local

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// sessionsAvailable reports whether interactive process sessions can be
// started on this platform.
func sessionsAvailable() bool { return false }

// spawnProcess is the non-unix Runner starter: the registry stays
// functional, but every Start fails with NotAvailable rather than
// silently downgrading to a one-shot Exec.
func (r *Runner) spawnProcess(context.Context, sandbox.SessionSpec) (sandbox.Session, error) {
	return nil, errdefs.NotAvailablef(
		"sandbox: interactive process sessions require a unix platform")
}
