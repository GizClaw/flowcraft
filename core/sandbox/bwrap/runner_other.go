//go:build !linux

package bwrap

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// Runner is the non-Linux stub of the bubblewrap backend.
type Runner struct{}

// New always returns errdefs.NotAvailable on non-Linux platforms.
func New(rootDir string, opts ...RunnerOption) (*Runner, error) {
	_ = rootDir
	_ = opts
	return nil, errdefs.NotAvailablef(
		"bwrap: backend requires Linux; not available on this platform")
}

// Capabilities reports no capabilities on unsupported platforms.
func (*Runner) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{Policy: sandbox.Enforcement{}}
}

// Start is unreachable because New never returns a non-nil Runner.
func (*Runner) Start(context.Context, sandbox.SessionSpec) (sandbox.Session, error) {
	return nil, errdefs.NotAvailablef("bwrap: not available on this platform")
}

// List is unreachable outside Linux.
func (*Runner) List(context.Context) ([]sandbox.SessionInfo, error) {
	return nil, errdefs.NotAvailablef("bwrap: not available on this platform")
}

// Terminate is unreachable outside Linux.
func (*Runner) Terminate(context.Context, string) error {
	return errdefs.NotAvailablef("bwrap: not available on this platform")
}

// Close is unreachable outside Linux.
func (*Runner) Close() error {
	return errdefs.NotAvailablef("bwrap: not available on this platform")
}
