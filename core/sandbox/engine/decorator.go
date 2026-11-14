package engine

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// AllowCommands returns a Runner that delegates to inner only when the
// command's exact name appears in allowed; every other command is
// rejected before reaching inner. It is the functional replacement for
// the v0.1 ScopedCommandRunner type — a decorator rather than a struct
// with exported fields. Matching is on the full command string passed to
// Exec; callers that want to match base names (so "/usr/bin/echo"
// matches "echo") should normalise before invoking Exec.
func AllowCommands(inner sandbox.Runner, allowed []string) sandbox.Runner {
	wl := make(map[string]bool, len(allowed))
	for _, cmd := range allowed {
		wl[cmd] = true
	}
	return &allowCommandsRunner{inner: inner, whitelist: wl}
}

type allowCommandsRunner struct {
	inner     sandbox.Runner
	whitelist map[string]bool
}

func (r *allowCommandsRunner) Exec(ctx context.Context, cmd string, args []string, opts sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	if !r.whitelist[cmd] {
		return nil, errdefs.PolicyDeniedf("sandbox: command %q is not in the whitelist", cmd)
	}
	return Exec(ctx, r.inner, cmd, args, opts)
}

// Start implements Runner with the same gate as Exec: the
// session's Argv[0] must be whitelisted before it is ever spawned.
// Note this gates the session start, not input typed inside an
// interactive shell — that is the documented all-or-nothing trade-off
// of interactive sessions.
func (r *allowCommandsRunner) Start(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	if len(spec.Argv) == 0 {
		return nil, errdefs.Validationf("sandbox: SessionSpec.Argv must name a command")
	}
	if !r.whitelist[spec.Argv[0]] {
		return nil, errdefs.PolicyDeniedf(
			"sandbox: command %q is not in the whitelist", spec.Argv[0])
	}
	return r.inner.Start(ctx, spec)
}

// List forwards the inner runner.s session list.
func (r *allowCommandsRunner) List(ctx context.Context) ([]sandbox.SessionInfo, error) {
	return r.inner.List(ctx)
}

// Terminate forwards to the inner runner's session manager.
func (r *allowCommandsRunner) Terminate(ctx context.Context, id string) error {
	return r.inner.Terminate(ctx, id)
}

// Capabilities forwards the inner runner's declaration: gating command
// names narrows what may run but adds no enforcement or session
// capability, so the decorator claims exactly what its inner runner
// declares.
func (r *allowCommandsRunner) Capabilities() sandbox.Capabilities {
	return r.inner.Capabilities()
}

// Close forwards to the inner runner so lifecycle survives decoration.
func (r *allowCommandsRunner) Close() error {
	return r.inner.Close()
}

// OpenJournal forwards to the inner runner: gating command names does
// not change what the backend can observe, so the decorated runner
// exposes exactly the journal its inner runner exposes (and
// NotAvailable when there is none).
func (r *allowCommandsRunner) OpenJournal(ctx context.Context) (sandbox.FileJournal, error) {
	return sandbox.OpenJournal(ctx, r.inner)
}

// WithDefaults returns a Runner that merges defaults into every Exec
// call's ExecOptions before delegating to inner. It is the
// composition seam that lets a runtime owner (typically a host
// application instantiating a sandbox resource) fix the
// application-level shared policy — env allow-list, network mode,
// resource caps — onto a Runner that callers (tools, scripts) then
// invoke with only behavioural knobs (cwd, stdin, per-call timeout).
//
// Merge semantics are deliberately security-biased: policy fields
// belong to defaults, behavioural fields belong to the caller.
// A tool cannot escape sandbox policy by passing wider ExecOptions
// at call time.
//
//   - WorkDir: caller wins. Empty caller WorkDir falls back to
//     defaults.WorkDir.
//   - Stdin: caller wins. nil caller Stdin falls back to
//     defaults.Stdin (rare in practice; here for symmetry). It is
//     Exec-only: Start never delivers those bytes and never treats
//     them as an input side, so a merged-into-Start spec still runs on
//     /dev/null and Write reports NotAvailable.
//   - Timeout: min(caller, defaults) when both > 0. A non-zero side
//     overrides a zero side. Zero on both sides means "no
//     sandbox-imposed timeout"; the caller's ctx still applies. The
//     min rule lets defaults act as a ceiling — a tool can ask for
//     a shorter window than the sandbox grants, never a longer one.
//   - Env.Allow: defaults wins entirely. A non-nil caller Allow is
//     ignored; widening the host-env allow-list at call time would
//     defeat the sandbox. Callers that want a narrower view should
//     not run as exec at all, or should be deployed against a
//     differently-configured Sandbox resource.
//   - Env.Inject: union; caller entries override defaults on key
//     collision. This is the one place a tool can layer in
//     per-call context (RUN_ID, REQUEST_ID, ...) on top of the
//     sandbox's static injections.
//   - Net: defaults wins entirely. Caller Net is ignored — the
//     network posture is sandbox-level policy.
//   - Permission: a caller-supplied boundary replaces the configured
//     default for that call, in either direction of strictness, because
//     the caller stated the decision. The default applies only when the
//     caller supplied nothing. A default never widens a caller policy;
//     an invalid policy is preserved so backend validation rejects it
//     instead of it being silently dropped.
//   - Resources: field-wise, the stricter of the two — the smaller
//     non-zero value, with a zero side deferring to the other (the same
//     rule Timeout uses). Defaults therefore act as a ceiling and a call
//     may ask for a tighter cap, but a decorator never silently drops a
//     caller's cap: dropping one would run the command with less
//     enforcement than the caller asked for, which is the one thing a
//     boundary must not do.
//
// Composition with the other decorators:
//
//	rn := sandbox.WithDefaults(
//	    sandbox.AllowCommands(
//	        local.New(spec.Root, local.WithMaxOutputBytes(spec.MaxOutput)),
//	        spec.AllowedCommands,
//	    ),
//	    sandbox.ExecOptions{
//	        Env:       toEnvPolicy(spec.Env),
//	        Net:       toNetPolicy(spec.Net),
//	        Resources: toResourceLimits(spec.Resources),
//	    },
//	)
//
// The inner-to-outer ordering is: local.Runner (actually runs the
// command) → AllowCommands (gates the command name) → WithDefaults
// (rewrites ExecOptions). Reversing the gate vs. defaults order
// has no semantic difference today because neither decorator
// observes the other's domain.
func WithDefaults(inner sandbox.Runner, defaults sandbox.ExecOptions) sandbox.Runner {
	return &defaultsRunner{inner: inner, defaults: defaults}
}

type defaultsRunner struct {
	inner    sandbox.Runner
	defaults sandbox.ExecOptions
}

func (r *defaultsRunner) Exec(ctx context.Context, cmd string, args []string, opts sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	return Exec(ctx, r.inner, cmd, args, r.merge(opts))
}

// Start implements Runner: the session's Opts go through the
// same security-biased merge as Exec, so daemon-owned Env/Net/
// Resources policy is fixed before the backend ever sees the spawn.
func (r *defaultsRunner) Start(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	spec.Opts = r.merge(spec.Opts)
	return r.inner.Start(ctx, spec)
}

// List forwards the inner runner.s session list.
func (r *defaultsRunner) List(ctx context.Context) ([]sandbox.SessionInfo, error) {
	return r.inner.List(ctx)
}

// Terminate forwards to the inner runner's session manager.
func (r *defaultsRunner) Terminate(ctx context.Context, id string) error {
	return r.inner.Terminate(ctx, id)
}

// Capabilities forwards the inner runner's declaration: fixing policy
// defaults onto calls does not change what the backend can enforce or
// which session features it provides.
func (r *defaultsRunner) Capabilities() sandbox.Capabilities {
	return r.inner.Capabilities()
}

// Close forwards to the inner runner so lifecycle survives decoration.
func (r *defaultsRunner) Close() error {
	return r.inner.Close()
}

// OpenJournal forwards to the inner runner: merging default options
// into Exec calls does not change what the backend watches, so the
// decorated runner exposes exactly the inner journal (or NotAvailable
// when there is none).
func (r *defaultsRunner) OpenJournal(ctx context.Context) (sandbox.FileJournal, error) {
	return sandbox.OpenJournal(ctx, r.inner)
}

// merge applies the rules documented on [WithDefaults]. Kept on a
// pointer receiver so the defaults map is not copied on every call.
func (r *defaultsRunner) merge(caller sandbox.ExecOptions) sandbox.ExecOptions {
	merged := sandbox.ExecOptions{
		WorkDir:    caller.WorkDir,
		Stdin:      caller.Stdin,
		Timeout:    mergeTimeout(caller.Timeout, r.defaults.Timeout),
		Env:        mergeEnv(caller.Env, r.defaults.Env),
		Net:        r.defaults.Net,
		Permission: caller.Permission,
		Resources:  mergeResources(caller.Resources, r.defaults.Resources),
	}
	// A caller policy is a decision, so defaults never widen it; when the
	// caller supplied none, the configured boundary applies.
	if merged.Permission == nil {
		merged.Permission = r.defaults.Permission
	}
	if merged.WorkDir == "" {
		merged.WorkDir = r.defaults.WorkDir
	}
	if merged.Stdin == nil {
		merged.Stdin = r.defaults.Stdin
	}
	return merged
}

// mergeTimeout takes the smaller of the two positive durations.
// A zero side defers to the other side; both zero means no
// sandbox-imposed timeout.
func mergeTimeout(caller, def time.Duration) time.Duration {
	if caller > 0 && def > 0 {
		if caller < def {
			return caller
		}
		return def
	}
	if caller > 0 {
		return caller
	}
	return def
}

// mergeResources takes the stricter of the two budgets field by field: a zero
// side defers to the other, and when both are set the smaller wins. It is the same
// shape as mergeTimeout, because a resource cap is a ceiling rather than a
// posture: the caller may ask for less, never for more, and the decorator must not
// discard the caller's value on the way down.
func mergeResources(caller, defaults sandbox.ResourceLimits) sandbox.ResourceLimits {
	return sandbox.ResourceLimits{
		CPUMillicores:  minPositive(caller.CPUMillicores, defaults.CPUMillicores),
		MemoryBytes:    minPositive64(caller.MemoryBytes, defaults.MemoryBytes),
		DiskBytes:      minPositive64(caller.DiskBytes, defaults.DiskBytes),
		MaxOutputBytes: minPositive64(caller.MaxOutputBytes, defaults.MaxOutputBytes),
	}
}

func minPositive(a, b int) int {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

func minPositive64(a, b int64) int64 {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// mergeEnv enforces the asymmetric Env policy: defaults owns Allow,
// caller can layer Inject. We never clone defaults.Inject directly
// into the returned EnvPolicy when caller.Inject is empty — the
// local.Runner reads the map by value, and aliasing the defaults map
// would let a buggy downstream mutate the shared policy.
func mergeEnv(caller, def sandbox.EnvPolicy) sandbox.EnvPolicy {
	out := sandbox.EnvPolicy{Allow: def.Allow}
	if len(def.Inject) == 0 && len(caller.Inject) == 0 {
		return out
	}
	merged := make(map[string]string, len(def.Inject)+len(caller.Inject))
	for k, v := range def.Inject {
		merged[k] = v
	}
	for k, v := range caller.Inject {
		merged[k] = v
	}
	out.Inject = merged
	return out
}
