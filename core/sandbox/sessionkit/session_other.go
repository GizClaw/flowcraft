//go:build !unix && !windows

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

// SessionFeatures reports what a session supports where there is no session
// implementation: nothing.
func SessionFeatures() sandbox.SessionFeatures { return sandbox.SessionFeatures{} }

// isBrokenPipe is the fallback half of [isClosedInput]: a platform with no
// session implementation has no pipe errno to match, so only the portable
// io.ErrClosedPipe / os.ErrClosed cases apply.
func isBrokenPipe(error) bool { return false }
