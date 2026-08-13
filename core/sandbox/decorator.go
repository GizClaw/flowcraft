package sandbox

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// AllowCommands returns a Runner that delegates to inner only when the
// command's exact name appears in allowed; every other command is
// rejected before reaching inner. It is the functional replacement for
// the v0.1 ScopedCommandRunner type — a decorator rather than a struct
// with exported fields. Matching is on the full command string passed to
// Exec; callers that want to match base names (so "/usr/bin/echo"
// matches "echo") should normalise before invoking Exec.
func AllowCommands(inner Runner, allowed []string) Runner {
	wl := make(map[string]bool, len(allowed))
	for _, cmd := range allowed {
		wl[cmd] = true
	}
	return &allowCommandsRunner{inner: inner, whitelist: wl}
}

type allowCommandsRunner struct {
	inner     Runner
	whitelist map[string]bool
}

func (r *allowCommandsRunner) Exec(ctx context.Context, cmd string, args []string, opts ExecOptions) (*ExecResult, error) {
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
func (r *allowCommandsRunner) Start(ctx context.Context, spec SessionSpec) (Session, error) {
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
func (r *allowCommandsRunner) List(ctx context.Context) ([]SessionInfo, error) {
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
func (r *allowCommandsRunner) Capabilities() Capabilities {
	return r.inner.Capabilities()
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
//     defaults.Stdin (rare in practice; here for symmetry).
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
//   - Resources: defaults wins entirely. Caller cannot raise caps;
//     and narrowing CPU/Mem/Disk per call is not actionable for a
//     LocalRunner today (those fields are advisory until a real
//     isolation backend lands), so the simpler "defaults only"
//     rule keeps the contract honest.
//
// Composition with the other decorators:
//
//	rn := sandbox.WithDefaults(
//	    sandbox.AllowCommands(
//	        sandbox.NewLocalRunner(spec.Root, sandbox.WithMaxOutputBytes(spec.MaxOutput)),
//	        spec.AllowedCommands,
//	    ),
//	    sandbox.ExecOptions{
//	        Env:       toEnvPolicy(spec.Env),
//	        Net:       toNetPolicy(spec.Net),
//	        Resources: toResourceLimits(spec.Resources),
//	    },
//	)
//
// The inner-to-outer ordering is: LocalRunner (actually runs the
// command) → AllowCommands (gates the command name) → WithDefaults
// (rewrites ExecOptions). Reversing the gate vs. defaults order
// has no semantic difference today because neither decorator
// observes the other's domain.
func WithDefaults(inner Runner, defaults ExecOptions) Runner {
	return &defaultsRunner{inner: inner, defaults: defaults}
}

type defaultsRunner struct {
	inner    Runner
	defaults ExecOptions
}

func (r *defaultsRunner) Exec(ctx context.Context, cmd string, args []string, opts ExecOptions) (*ExecResult, error) {
	return Exec(ctx, r.inner, cmd, args, r.merge(opts))
}

// Start implements Runner: the session's Opts go through the
// same security-biased merge as Exec, so daemon-owned Env/Net/
// Resources policy is fixed before the backend ever sees the spawn.
func (r *defaultsRunner) Start(ctx context.Context, spec SessionSpec) (Session, error) {
	spec.Opts = r.merge(spec.Opts)
	return r.inner.Start(ctx, spec)
}

// List forwards the inner runner.s session list.
func (r *defaultsRunner) List(ctx context.Context) ([]SessionInfo, error) {
	return r.inner.List(ctx)
}

// Terminate forwards to the inner runner's session manager.
func (r *defaultsRunner) Terminate(ctx context.Context, id string) error {
	return r.inner.Terminate(ctx, id)
}

// Capabilities forwards the inner runner's declaration: fixing policy
// defaults onto calls does not change what the backend can enforce or
// which session features it provides.
func (r *defaultsRunner) Capabilities() Capabilities {
	return r.inner.Capabilities()
}

// merge applies the rules documented on [WithDefaults]. Kept on a
// pointer receiver so the defaults map is not copied on every call.
func (r *defaultsRunner) merge(caller ExecOptions) ExecOptions {
	merged := ExecOptions{
		WorkDir:   caller.WorkDir,
		Stdin:     caller.Stdin,
		Timeout:   mergeTimeout(caller.Timeout, r.defaults.Timeout),
		Env:       mergeEnv(caller.Env, r.defaults.Env),
		Net:       r.defaults.Net,
		Resources: r.defaults.Resources,
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

// mergeEnv enforces the asymmetric Env policy: defaults owns Allow,
// caller can layer Inject. We never clone defaults.Inject directly
// into the returned EnvPolicy when caller.Inject is empty — the
// LocalRunner reads the map by value, and aliasing the defaults map
// would let a buggy downstream mutate the shared policy.
func mergeEnv(caller, def EnvPolicy) EnvPolicy {
	out := EnvPolicy{Allow: def.Allow}
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
