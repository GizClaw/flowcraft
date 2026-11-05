package engine

import (
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/internal/session"
)

// ValidateExecPolicy rejects an exec policy the platform cannot honor: a write
// mode it does not define, a disk limit with no quota mechanism, a cpu cap with
// no timeout to derive it from, or resource caps where process-group sampling is
// unavailable. Backends call it before starting anything.
func ValidateExecPolicy(opts sandbox.ExecOptions) error {
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
	if (opts.Resources.MemoryBytes > 0 || opts.Resources.CPUMillicores > 0) && !session.GroupCapsAvailable() {
		return errdefs.NotAvailablef(
			"sandbox: resource limits require process-group sampling, which is unavailable here")
	}
	return nil
}
