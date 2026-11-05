//go:build !windows

package windows

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// Runner is the non-Windows stub of the windows backend.
type Runner struct{}

// New always returns errdefs.NotAvailable on non-Windows platforms.
func New(rootDir string, opts ...Option) (*Runner, error) {
	_ = rootDir
	_ = opts
	return nil, errdefs.NotAvailablef(
		"windows: backend requires Windows; not available on this platform")
}

// Capabilities reports no capabilities on unsupported platforms.
func (*Runner) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{Policy: sandbox.Enforcement{}}
}

// Start is unreachable because New never returns a non-nil Runner.
func (*Runner) Start(context.Context, sandbox.SessionSpec) (sandbox.Session, error) {
	return nil, errdefs.NotAvailablef("windows: not available on this platform")
}

// List is unreachable outside Windows.
func (*Runner) List(context.Context) ([]sandbox.SessionInfo, error) {
	return nil, errdefs.NotAvailablef("windows: not available on this platform")
}

// Terminate is unreachable outside Windows.
func (*Runner) Terminate(context.Context, string) error {
	return errdefs.NotAvailablef("windows: not available on this platform")
}

// Close is unreachable outside Windows.
func (*Runner) Close() error {
	return errdefs.NotAvailablef("windows: not available on this platform")
}
