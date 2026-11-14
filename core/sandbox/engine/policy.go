package engine

import (
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// ValidateExecPolicy rejects an exec policy the platform cannot honor: a write
// mode it does not define, a disk limit with no quota mechanism, a cpu cap with
// no timeout to derive it from, or resource caps the runner does not enforce.
// Backends call it before starting anything.
//
// caps is the caller's own [sandbox.Capabilities].Policy — the same value it
// reports from Capabilities — so "can this backend cap memory/cpu" is answered
// by the backend's declaration instead of by a platform probe in the shared
// layer. That keeps this package free of any dependency on how processes are
// actually supervised (pty handling, the group sampler, the session registry),
// which is what lets a backend live outside core/sandbox.
func ValidateExecPolicy(opts sandbox.ExecOptions, caps sandbox.Enforcement) error {
	if opts.Permission != nil {
		// The network posture has its own field: a nil Permission must mean
		// "inherit the runner boundary" while a nil network means
		// "unrestricted", and one pointer cannot carry both zero meanings.
		if !opts.Permission.Net.IsZero() {
			return errdefs.Validationf(
				"sandbox: set the network posture on ExecOptions.Net, not on Permission.Net")
		}
		if err := opts.Permission.Validate(); err != nil {
			return err
		}
	}
	if opts.Resources.DiskBytes != 0 {
		return errdefs.NotAvailablef(
			"sandbox: disk limits not supported (no quota mechanism)")
	}
	if opts.Resources.CPUMillicores != 0 && opts.Timeout <= 0 {
		return errdefs.NotAvailablef(
			"sandbox: CPUMillicores requires a per-call Timeout to derive a cpu-time cap")
	}
	if opts.Resources.MemoryBytes > 0 && !caps.MemoryCap {
		return errdefs.NotAvailablef(
			"sandbox: memory limits are not enforced by this runner")
	}
	if opts.Resources.CPUMillicores > 0 && !caps.CPUCap {
		return errdefs.NotAvailablef(
			"sandbox: cpu limits are not enforced by this runner")
	}
	return nil
}
