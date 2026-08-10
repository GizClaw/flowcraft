package bwrap

import (
	"github.com/GizClaw/flowcraft/sdkx/internal/httpkit"
	"github.com/GizClaw/flowcraft/sdkx/internal/httpkit/mitm"
)

// RunnerOption configures a Runner at construction time.
type RunnerOption func(*runnerConfig)

// runnerConfig is the resolved set of options shared between platforms.
// It lives in the platform-neutral file so the option functions
// type-check on every OS even though Runner itself is Linux-only.
type runnerConfig struct {
	binFrom  string   // raw value supplied to WithBinary, "" if defaulted
	extra    []string // extra bwrap flags injected before the "--" separator
	writable []string // additional explicitly writable host paths
	decision func(httpkit.ProxyDecision)
	hooks    mitm.ProxyHooks
}

// WithBinary overrides the bwrap binary path. By default the Runner
// uses exec.LookPath("bwrap"); set this for hermetic builds where
// bwrap lives in a vendored directory.
func WithBinary(path string) RunnerOption {
	return func(c *runnerConfig) {
		c.binFrom = path
	}
}

// WithExtraFlags injects extra arguments between the auto-generated
// flag list and the "--" separator that precedes the command. Use
// sparingly: per-policy flags are owned by sandbox.ExecOptions.
//
// Flags that can weaken the boundary are rejected by New. That
// includes every mount / env / net / namespace / workdir / seccomp
// option bwrap exposes, so the escape hatch cannot downgrade any
// policy dimension the Runner promises to enforce. Extras are also
// emitted before the policy flags so policy flags always win.
// Use [WithWritablePaths] for intentional write exceptions.
func WithExtraFlags(flags ...string) RunnerOption {
	return func(c *runnerConfig) {
		c.extra = append(c.extra, flags...)
	}
}

// WithWritablePaths grants write access to additional existing paths
// beyond rootDir. The rest of the host filesystem remains visible but
// read-only. Use this for dedicated build caches; do not grant shared
// roots such as /tmp or $HOME wholesale. Paths are resolved through
// EvalSymlinks at construction.
func WithWritablePaths(paths ...string) RunnerOption {
	return func(c *runnerConfig) {
		c.writable = append(c.writable, paths...)
	}
}

// WithProxyDecision installs the per-decision audit callback used by
// the host-side enforcement proxy. Keep it fast and non-throwing.
func WithProxyDecision(fn func(httpkit.ProxyDecision)) RunnerOption {
	return func(c *runnerConfig) {
		c.decision = fn
	}
}

// WithProxyHooks installs MITM observation/blocking hooks. They only
// fire when the sandbox policy enables MITM.
func WithProxyHooks(h mitm.ProxyHooks) RunnerOption {
	return func(c *runnerConfig) {
		c.hooks = h
	}
}
