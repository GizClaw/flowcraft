package bwrap

import (
	"crypto/x509"

	"github.com/GizClaw/flowcraft/core/utils/net"
)

// RunnerOption configures a Runner at construction time.
type RunnerOption func(*runnerConfig)

// runnerConfig is the resolved set of options shared between platforms.
// It lives in the platform-neutral file so the option functions
// type-check on every OS even though Runner itself is Linux-only.
type runnerConfig struct {
	binFrom      string   // raw value supplied to WithBinary, "" if defaulted
	extra        []string // extra bwrap flags injected before the "--" separator
	writable     []string // additional explicitly writable host paths
	readOnlyRoot bool     // keep the runner root read-only for every exec
	decision     func(net.ProxyDecision)
	hooks        net.MITMHooks
	roots        *x509.CertPool
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

// WithReadOnlyRoot keeps the runner root read-only for every exec
// spawned through this runner, instead of the default (root writable).
// Explicit [WithWritablePaths] exceptions remain writable, and
// per-exec sandbox.WriteReadOnly can only keep the root read-only —
// it cannot re-enable writes on a runner constructed with this option.
func WithReadOnlyRoot() RunnerOption {
	return func(c *runnerConfig) {
		c.readOnlyRoot = true
	}
}

// WithProxyDecision installs the per-decision audit callback used by
// the host-side enforcement proxy. Keep it fast and non-throwing.
func WithProxyDecision(fn func(net.ProxyDecision)) RunnerOption {
	return func(c *runnerConfig) {
		c.decision = fn
	}
}

// WithProxyHooks installs MITM observation/blocking hooks. They only
// fire when the sandbox policy enables MITM.
func WithProxyHooks(h net.MITMHooks) RunnerOption {
	return func(c *runnerConfig) {
		c.hooks = h
	}
}

// WithOutboundRoots overrides the roots used to verify the real
// target's TLS certificate during MITM (nil means system roots). Use
// it for custom/internal CAs; it never weakens the child side, which
// still trusts only the injected temporary CA plus system roots.
func WithOutboundRoots(roots *x509.CertPool) RunnerOption {
	return func(c *runnerConfig) {
		c.roots = roots
	}
}
