//go:build !unix

package sandbox

import (
	"context"
	"os/exec"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// StartSession is not available off unix: interactive sessions need
// pty(4), process groups, and signal delivery that have no portable
// Windows equivalent. Start fails with NotAvailable rather than a
// silent downgrade.
func StartSession(_ context.Context, _ SessionSpec, _ *exec.Cmd) (Session, error) {
	return nil, errdefs.NotAvailablef(
		"sandbox: interactive process sessions require a unix platform")
}
