//go:build !linux

package nsjail

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

// Runner is the non-Linux stub of the nsjail backend. It exists so
// non-Linux developers can import this package for type references
// (interfaces, option functions, tests for translation helpers)
// without build-tag gymnastics; instantiating one is intentionally
// impossible.
type Runner struct{}

// New always returns errdefs.NotAvailable on non-Linux platforms.
// nsjail uses Linux-specific namespace and cgroup primitives that
// have no counterpart on macOS or Windows. Callers that need a
// portable fallback should select sandbox.LocalRunner explicitly.
func New(rootDir string, opts ...RunnerOption) (*Runner, error) {
	// Touch the args so the compiler does not complain about unused
	// parameters when this stub is the chosen build.
	_ = rootDir
	_ = opts
	return nil, errdefs.NotAvailablef(
		"nsjail: backend requires Linux; not available on this platform")
}

// Exec is unreachable because New never returns a non-nil Runner on
// non-Linux platforms. It exists so *Runner satisfies sandbox.Runner
// in type assertions written by portable code.
func (*Runner) Exec(ctx context.Context, cmd string, args []string, opts sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	_, _, _, _ = ctx, cmd, args, opts
	return nil, errdefs.NotAvailablef("nsjail: not available on this platform")
}
