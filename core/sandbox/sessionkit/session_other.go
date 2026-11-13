//go:build !unix

package sessionkit

import (
	"context"
	"os/exec"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// StartSession is not available off unix: interactive sessions need
// pty(4), process groups, and signal delivery that have no portable
// Windows equivalent. Start fails with NotAvailable rather than a
// silent downgrade.
func StartSession(_ context.Context, _ sandbox.SessionSpec, _ *exec.Cmd) (sandbox.Session, error) {
	return nil, errdefs.NotAvailablef(
		"sandbox: interactive process sessions require a unix platform")
}
