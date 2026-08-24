//go:build windows

package windows

import (
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// validateExecPolicy is the Windows counterpart of
// sandbox.ValidateExecPolicy. The shared validator hard-gates caps on
// the unix ps(1) sampler (groupCapsAvailable), which cannot run on
// Windows; this one gates them on the Job Object mechanism instead.
// Everything else follows the same refuse-don't-downgrade rules.
func validateExecPolicy(opts sandbox.ExecOptions) error {
	if opts.Resources.DiskBytes != 0 {
		return errdefs.NotAvailablef(
			"windows: disk limits not supported (no quota mechanism)")
	}
	if opts.Resources.CPUMillicores != 0 && opts.Timeout <= 0 {
		return errdefs.NotAvailablef(
			"windows: CPUMillicores requires a per-call Timeout to derive a cpu-time cap")
	}
	if (opts.Resources.MemoryBytes > 0 || opts.Resources.CPUMillicores > 0) && !jobObjectCapsAvailable() {
		return errdefs.NotAvailablef(
			"windows: resource limits require job-object enforcement, which is unavailable here")
	}
	return nil
}
