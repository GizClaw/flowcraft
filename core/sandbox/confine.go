package sandbox

import (
	"errors"
	"fmt"
	"slices"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/policy"
)

// Confine is what a backend instance is built to enforce.
//
// It is a session's permission projected onto the dimensions a backend can act
// on: the egress posture, the environment allow-list, and the ceilings one command
// may spend. Everything here is fixed when the instance is created, because a
// backend serves exactly one session — so "the policy of this call" and "the
// policy of this session" are the same thing, and a per-call field would only
// re-open what it means to run a narrower call inside a wider boundary.
//
// Budgets are the exception, and they are why [ExecOptions] still carries
// [policy.Resources]: a call may ask for less than the ceiling. [Confine.Narrow]
// is the only place that decides, and it decides in one direction.
type Confine struct {
	// Files is the file boundary the backend must enforce for this instance: the read roots
	// it may expose and the write set it may make writable. It is the grant's own value,
	// unchanged — a backend that has to project it (mounts, a profile) projects the
	// permission itself, never a second spelling of it.
	Files policy.Files
	// Net is the outbound posture the backend enforces for this instance.
	Net policy.NetPolicy
	// Env is which host environment the child may see, plus the values the
	// policy injects. It is a permission, not a per-call argument.
	Env policy.EnvPolicy
	// Resources is the ceiling for one command, in the same shape a call uses to
	// ask for less.
	Resources policy.Resources
}

// ErrConfineDenied reports that a backend refused a command because running it
// would exceed the confine the instance was built for: a write outside the write
// set, a destination the egress posture forbids.
//
// It is the one refusal a host can act on. The command has not run, the boundary
// is intact, and the difference between "this command is outside what this
// session may do" and "this command ran and failed" is the difference between a
// rule and a bug — so the backend says which one it is instead of leaving the
// host to read a message.
var ErrConfineDenied = errors.New("sandbox: the command is outside this session's confine")

// Deniedf builds a confine refusal, naming what the command would have exceeded.
//
// The result answers two questions at once: [ErrConfineDenied] says the boundary
// refused before anything ran, and [errdefs.IsPolicyDenied] says a rule refused
// rather than an execution failing. Neither caller has to parse the message, and
// a backend that returns it does not have to import the host's vocabulary or the
// policy package's typed denial to be understood.
func Deniedf(format string, args ...any) error {
	return fmt.Errorf("%w: %w", ErrConfineDenied, errdefs.PolicyDeniedf(format, args...))
}

// ValidateConfine reports whether the backend can enforce what the instance was
// built for.
//
// It is the construction-time gate: a backend calls it once, before it opens
// anything, so a posture it cannot honour is an error where an operator sees it
// rather than a failure on the first command that needs it. caps is the backend's
// own declaration — the same value it reports from Capabilities — so "can this
// backend cap memory" is answered by the declaration instead of by a platform
// probe in the shared layer.
func ValidateConfine(confine Confine, caps Enforcement) error {
	if mode := confine.Net.Mode; mode != policy.NetDefault && !slices.Contains(caps.NetModes, mode) {
		return errdefs.NotAvailablef("sandbox: the backend cannot enforce network mode %q", mode)
	}
	if confine.Net.MITM != nil && confine.Net.MITM.Enabled && !caps.MITM {
		return errdefs.NotAvailablef("sandbox: the backend cannot terminate TLS for CONNECT traffic")
	}
	if len(confine.Net.UnixSockets) > 0 && !caps.UnixSocketPolicy {
		return errdefs.NotAvailablef("sandbox: the backend cannot confine unix socket egress")
	}
	if confine.Files.RestrictsWrites() && !caps.FilesystemBounds {
		return errdefs.NotAvailablef(
			"sandbox: the backend cannot confine writes; a read-only or narrowed write set needs one that can")
	}
	if confine.Env.Allow != nil && !caps.EnvAllowList {
		return errdefs.NotAvailablef(
			"sandbox: the backend cannot honour an environment allow-list; it would forward the whole host environment")
	}

	return ValidateBudgets(confine.Resources, caps)
}

// ValidateBudgets reports whether the backend can enforce the budgets a session asks for.
//
// It is the per-call half of [ValidateConfine], and it exists because [Confine.Narrow] cannot answer
// the question: a ceiling of zero means "unbounded", so a call may name a budget the instance never
// claimed. A backend calls this after narrowing and refuses what it cannot enforce, instead of running
// the command uncapped and reporting success — a budget that quietly lost its value is
// indistinguishable from one that never had it.
func ValidateBudgets(res policy.Resources, caps Enforcement) error {
	switch {
	case res.DiskBytes > 0 && !caps.DiskCap:
		return errdefs.NotAvailablef("sandbox: disk limits are not enforced by this backend")
	case res.MemoryBytes > 0 && !caps.MemoryCap:
		return errdefs.NotAvailablef("sandbox: memory limits are not enforced by this backend")
	case res.CPUMillicores > 0 && !caps.CPUCap:
		return errdefs.NotAvailablef("sandbox: cpu limits are not enforced by this backend")
	}
	return nil
}

// Narrow applies one call's request to the instance's ceilings and returns the
// options the backend should act on.
//
// A zero field in the request means "inherit": the call did not ask, so the
// instance's cap stands. A non-zero field must fit under the ceiling, and asking
// for more of something the instance bounds is an error rather than a silent
// truncation — a budget that quietly lost its value is indistinguishable from one
// that never had it.
//
// It returns the resolved options rather than only validating them, because the
// merge has to happen somewhere: a backend that validated and then forwarded the
// raw request would drop the instance's caps on every call that did not repeat
// them.
func (c Confine) Narrow(opts ExecOptions) (ExecOptions, error) {
	ceiling, requested := c.Resources, opts.Resources
	out := opts
	var err error

	if out.Resources.CPUMillicores, err = narrowed("cpu_millicores", ceiling.CPUMillicores, requested.CPUMillicores); err != nil {
		return ExecOptions{}, err
	}
	if out.Resources.MemoryBytes, err = narrowed("memory_bytes", ceiling.MemoryBytes, requested.MemoryBytes); err != nil {
		return ExecOptions{}, err
	}
	if out.Resources.DiskBytes, err = narrowed("disk_bytes", ceiling.DiskBytes, requested.DiskBytes); err != nil {
		return ExecOptions{}, err
	}
	if out.Resources.MaxOutputBytes, err = narrowed("max_output_bytes", ceiling.MaxOutputBytes, requested.MaxOutputBytes); err != nil {
		return ExecOptions{}, err
	}

	// A cpu budget is a rate: it is enforced as group cpu-time over the call's
	// deadline, so a deadline is what turns it into seconds of cpu. Nothing can
	// derive it from a call that names none.
	if out.Resources.CPUMillicores > 0 && opts.Timeout <= 0 {
		return ExecOptions{}, errdefs.NotAvailablef(
			"sandbox: a cpu budget requires a per-call deadline to derive it from")
	}
	return out, nil
}

// narrowed resolves one budget: an unrequested field inherits the ceiling, and a
// requested one must fit under it.
func narrowed[T int | int64](name string, ceiling, requested T) (T, error) {
	switch {
	case requested == 0:
		return ceiling, nil
	case ceiling > 0 && requested > ceiling:
		return 0, errdefs.NotAvailablef(
			"sandbox: %s %d exceeds this session's ceiling of %d", name, requested, ceiling)
	default:
		return requested, nil
	}
}
