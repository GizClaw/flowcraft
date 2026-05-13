package sandbox

import (
	"context"
	"fmt"
	"time"
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
		return nil, fmt.Errorf("sandbox: command %q is not in the whitelist", cmd)
	}
	return r.inner.Exec(ctx, cmd, args, opts)
}

// WithDefaults returns a Runner that merges defaults into every Exec
// call's ExecOptions before delegating to inner. It is the
// composition seam that lets a runtime owner (typically vesseld
// Catalog instantiating a kind: Sandbox resource) fix the
// daemon-level shared policy — env allow-list, network mode,
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
	return r.inner.Exec(ctx, cmd, args, r.merge(opts))
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

// NoopRunner is a zero-policy Runner that always returns an empty
// successful ExecResult. It is useful as a default in test wiring or as
// the inner Runner for AllowCommands when the caller wants to assert
// "the allow-list rejected the call" without actually running anything.
type NoopRunner struct{}

// Exec implements Runner. It ignores every argument and returns an empty
// ExecResult with nil error.
func (NoopRunner) Exec(_ context.Context, _ string, _ []string, _ ExecOptions) (*ExecResult, error) {
	return &ExecResult{}, nil
}
