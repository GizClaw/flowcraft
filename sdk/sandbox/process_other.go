//go:build !unix

package sandbox

import (
	"context"
	"os/exec"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// StartSession is not available off unix: interactive sessions need
// pty(4), process groups, and signal delivery that have no portable
// Windows equivalent. ProcessManagerOf still discovers the capability
// through ProcessManager, so callers see NotAvailable rather than a
// silent downgrade.
func StartSession(_ context.Context, _ *exec.Cmd, _ ExecOptions, _ bool, _, _ int) (Process, error) {
	return nil, errdefs.NotAvailablef(
		"sandbox: interactive process sessions require a unix platform")
}

// spawnProcess is the non-unix LocalRunner starter: the registry stays
// functional, but every Start fails with NotAvailable rather than
// silently downgrading to a one-shot Exec.
func (r *LocalRunner) spawnProcess(context.Context, ProcessSpec) (Process, error) {
	return nil, errdefs.NotAvailablef(
		"sandbox: interactive process sessions require a unix platform")
}
