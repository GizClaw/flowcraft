package resolver

import (
	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec/v1alpha1"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
	nsjailrunner "github.com/GizClaw/flowcraft/sdkx/sandbox/nsjail"
)

// resolveSandboxes materialises one [sandbox.Runner] per Sandbox
// document, wrapped in [sandbox.WithDefaults] so the spec's
// EnvPolicy / NetPolicy / ResourceLimits become the floor that
// per-call ExecOptions cannot widen. Returns a name → Runner map
// plus an Errors aggregate so the resolver pattern (surface every
// per-document failure in one pass) is preserved.
//
// One Runner per Sandbox is sufficient at v0.2.0 because every
// backend in scope (local, nsjail) is goroutine-safe — the same
// instance handles concurrent Exec calls from multiple Captains.
// If a future backend turns out to be single-flight, this is the
// layer that knows about the constraint and can fan out to a
// per-Captain instance there.
func resolveSandboxes(sandboxes map[string]v1alpha1.Sandbox) (map[string]sandbox.Runner, *Errors) {
	errs := &Errors{}
	out := make(map[string]sandbox.Runner, len(sandboxes))
	for name, s := range sandboxes {
		runner, err := buildSandboxRunner(s)
		if err != nil {
			errs.add(err)
			continue
		}
		out[name] = runner
	}
	return out, errs
}

// buildSandboxRunner selects the backend and layers WithDefaults
// on top. The defaults snapshot here is taken from the spec
// verbatim; any future "interpolate secrets / inject runtime vars"
// step would land between the snapshot and the WithDefaults call.
func buildSandboxRunner(s v1alpha1.Sandbox) (sandbox.Runner, error) {
	base, err := buildSandboxBackend(s)
	if err != nil {
		return nil, err
	}
	defaults := sandboxDefaultsFromSpec(s.Spec)
	if isZeroExecOptions(defaults) {
		// No daemon-level policy → return the base runner
		// directly. Avoids the WithDefaults wrapper allocation
		// and surfaces a clearer call stack in the error path
		// when nothing here is actually decorating the runner.
		return base, nil
	}
	return sandbox.WithDefaults(base, defaults), nil
}

// buildSandboxBackend instantiates the raw runner. Linux-only
// backends (nsjail) surface errdefs.NotAvailable on non-Linux
// hosts via the runner's own New constructor; we propagate that
// faithfully so an operator developing on macOS sees a clear "this
// backend is Linux-only" message at boot rather than at first exec.
func buildSandboxBackend(s v1alpha1.Sandbox) (sandbox.Runner, error) {
	switch s.Spec.Backend {
	case "local":
		return sandbox.NewLocalRunner(s.Spec.RootDir), nil
	case "nsjail":
		opts := []nsjailrunner.RunnerOption{}
		if s.Spec.Nsjail != nil {
			if s.Spec.Nsjail.Binary != "" {
				opts = append(opts, nsjailrunner.WithBinary(s.Spec.Nsjail.Binary))
			}
			if len(s.Spec.Nsjail.ExtraFlags) > 0 {
				opts = append(opts, nsjailrunner.WithExtraFlags(s.Spec.Nsjail.ExtraFlags...))
			}
		}
		r, err := nsjailrunner.New(s.Spec.RootDir, opts...)
		if err != nil {
			// nsjail.New surfaces errdefs.NotAvailable on
			// non-Linux hosts (and when the binary is missing);
			// propagate the wrapped error so the operator sees
			// the original classification.
			return nil, err
		}
		return r, nil
	default:
		// apispec.Validate already covers this — defensive only.
		return nil, errdefs.Validationf("vesseld Sandbox %q: unknown backend %q", s.Name, s.Spec.Backend)
	}
}

// sandboxDefaultsFromSpec projects the YAML SandboxSpec onto the
// sandbox.ExecOptions shape WithDefaults consumes. The projection
// is purely structural — every field maps 1:1 — so the SandboxSpec
// is essentially a wire-formatted ExecOptions with backend
// discrimination on top.
func sandboxDefaultsFromSpec(spec v1alpha1.SandboxSpec) sandbox.ExecOptions {
	// RootDir is consumed by the runner constructor itself, not
	// by ExecOptions.WorkDir — so it does NOT belong in defaults.
	// Per-call ExecOptions.WorkDir resolves relative to that
	// rootDir at exec time.
	opts := sandbox.ExecOptions{}
	if spec.Env != nil {
		opts.Env = sandbox.EnvPolicy{
			Allow:  spec.Env.Allow,
			Inject: spec.Env.Inject,
		}
	}
	if spec.Net != nil {
		opts.Net = sandbox.NetPolicy{
			Mode:       mapNetMode(spec.Net.Mode),
			AllowHosts: spec.Net.Allow,
			Proxy:      spec.Net.Proxy,
		}
	}
	if spec.Resources != nil {
		opts.Resources = sandbox.ResourceLimits{
			CPUMillicores:  spec.Resources.CPUMillicores,
			MemoryBytes:    spec.Resources.MemoryBytes,
			DiskBytes:      spec.Resources.DiskBytes,
			MaxOutputBytes: spec.Resources.MaxOutputBytes,
		}
	}
	return opts
}

// mapNetMode translates the YAML mode string into the sandbox
// constant. Empty → NetDefault, which mirrors the apispec
// convention "unset means the implementation's default".
func mapNetMode(mode string) sandbox.NetMode {
	switch mode {
	case "", "default":
		return sandbox.NetDefault
	case "deny-all":
		return sandbox.NetDenyAll
	case "allow-list":
		return sandbox.NetAllowList
	case "proxy":
		return sandbox.NetProxy
	default:
		// apispec validator catches this before resolver runs.
		return sandbox.NetDefault
	}
}

// isZeroExecOptions returns true when no policy field would change
// the runner's behaviour. Skipping WithDefaults in that case keeps
// the call stack shallow for the "Sandbox declared backend only"
// case (which is the common path for dev / local).
func isZeroExecOptions(o sandbox.ExecOptions) bool {
	if o.WorkDir != "" {
		return false
	}
	if len(o.Env.Allow) > 0 || len(o.Env.Inject) > 0 {
		return false
	}
	if o.Net.Mode != sandbox.NetDefault || len(o.Net.AllowHosts) > 0 || o.Net.Proxy != "" {
		return false
	}
	r := o.Resources
	if r.CPUMillicores != 0 || r.MemoryBytes != 0 || r.DiskBytes != 0 || r.MaxOutputBytes != 0 {
		return false
	}
	return true
}
