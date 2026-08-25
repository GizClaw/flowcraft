package sandbox

import (
	"errors"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// EnvPolicy controls which host environment variables a child process
// can observe, and lets the caller inject extra variables on top.
//
//   - Allow == nil: inherit the full host environment (back-compat with
//     the pre-sandbox behaviour of LocalCommandRunner).
//   - Allow == []string{} (non-nil empty slice): inherit nothing; the
//     child only sees the names listed in Inject.
//   - Allow == []string{"PATH", "HOME", ...}: only those names are
//     forwarded from the host; everything else is dropped.
//
// Inject is applied on top of the allow-list. Names in Inject win over
// host values of the same name.
type EnvPolicy struct {
	Allow  []string
	Inject map[string]string
}

// WritePolicy controls how much filesystem write access a single Exec
// call gets on top of the runner's construction-time boundary (runner
// root plus explicit [RunnerOption] writable paths).
//
//   - WriteWorkspace (zero value): the runner root plus explicit
//     writable paths are writable — the current behavior and the
//     back-compat default. With the seatbelt / bwrap backends this is
//     enforced at the OS level; sandbox/local has no OS boundary and
//     reports no write modes in Capabilities.
//   - WriteReadOnly: the runner root is not writable for this call.
//     Explicitly configured writable paths remain writable, and
//     platform escape hatches such as /dev/null stay open.
//
// There is deliberately no "wider" value: a call can narrow the
// construction-time boundary, never widen it. A runner constructed
// read-only (e.g. WithReadOnlyRoot) stays read-only for every call.
type WritePolicy uint8

const (
	WriteWorkspace WritePolicy = iota
	WriteReadOnly
)

// ResourceLimits caps how much the child process may consume.
//
// MemoryBytes caps aggregate resident memory used by the child process
// group. The local runner enforces it with its unix group watcher;
// sandboxed backends may use cgroups or VM caps instead.
//
// CPUMillicores expresses a cpu-time budget in thousandths of a core:
// backends derive a hard cap from it (sandbox/local: aggregate group
// cpu-time = Timeout x millicores/1000 via its sampling watcher).
// Because the budget is derived from the wall-clock timeout,
// The local runner requires Timeout > 0 when CPUMillicores is set and
// returns errdefs.NotAvailable otherwise.
//
// DiskBytes needs a quota mechanism no local backend has today; any
// non-zero value is rejected with errdefs.NotAvailable everywhere.
//
// MaxOutputBytes caps the bytes captured into ExecResult.Stdout and
// ExecResult.Stderr independently; excess output is dropped silently
// (the child process is not killed). The local runner enforces this directly.
// When zero, the runner's default applies (see local.WithMaxOutputBytes
// WithMaxOutputBytes option).
type ResourceLimits struct {
	CPUMillicores  int
	MemoryBytes    int64
	DiskBytes      int64
	MaxOutputBytes int64
}

// ValidateExecPolicy runs the policy checks every built-in backend
// applies before spawning anything, whether through Exec or
// Runner.Start:
//
//   - DiskBytes is rejected everywhere (no backend has a quota
//     mechanism yet).
//   - CPUMillicores derives its budget from Timeout, so it is rejected
//     when Timeout is absent.
//   - MemoryBytes / CPUMillicores ride the shared process-group
//     sampler; where that sampler cannot run, honouring the request
//     would silently run without caps, so it is rejected instead.
//   - Write must be a known WritePolicy value; anything else is
//     rejected so an unsupported mode cannot silently degrade to the
//     runner default.
//
// Backend-specific posture checks (which Net modes a runner enforces,
// WorkDir confinement) stay in each backend.
func ValidateExecPolicy(opts ExecOptions) error {
	if opts.Write > WriteReadOnly {
		return errdefs.Validationf(
			"sandbox: unknown write policy %d", int(opts.Write))
	}
	if opts.Resources.DiskBytes != 0 {
		return errdefs.NotAvailablef(
			"sandbox: disk limits not supported (no quota mechanism)")
	}
	if opts.Resources.CPUMillicores != 0 && opts.Timeout <= 0 {
		return errdefs.NotAvailablef(
			"sandbox: CPUMillicores requires a per-call Timeout to derive a cpu-time cap")
	}
	if (opts.Resources.MemoryBytes > 0 || opts.Resources.CPUMillicores > 0) && !groupCapsAvailable() {
		return errdefs.NotAvailablef(
			"sandbox: resource limits require process-group sampling, which is unavailable here")
	}
	return nil
}

// ErrPathTraversal is returned when a WorkDir resolves outside the
// runner's root, including via symlinks. sandbox owns its own
// ErrPathTraversal so this package does not depend on core/workspace
// (which would create an import cycle through the deprecation aliases).
// core/workspace keeps a separate ErrPathTraversal for its filesystem
// API.
var ErrPathTraversal = errdefs.Forbidden(errors.New("sandbox: path traversal denied"))
