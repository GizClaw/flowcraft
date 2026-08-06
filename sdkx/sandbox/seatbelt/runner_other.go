//go:build !darwin

package seatbelt

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

// Runner is the non-macOS stub of the Seatbelt backend.
type Runner struct{}

// New always returns errdefs.NotAvailable outside macOS.
func New(rootDir string, opts ...RunnerOption) (*Runner, error) {
	_ = rootDir
	_ = opts
	return nil, errdefs.NotAvailablef(
		"seatbelt: backend requires macOS; not available on this platform")
}

// Exec is unreachable because New cannot construct a Runner.
func (*Runner) Exec(ctx context.Context, cmd string, args []string, opts sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	_, _, _, _ = ctx, cmd, args, opts
	return nil, errdefs.NotAvailablef("seatbelt: not available on this platform")
}

// Enforcement reports the conservative zero value on unsupported
// platforms.
func (*Runner) Enforcement() sandbox.Enforcement {
	return sandbox.Enforcement{}
}
