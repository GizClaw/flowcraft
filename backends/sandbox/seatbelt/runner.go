package seatbelt

import (
	"crypto/x509"

	"github.com/GizClaw/flowcraft/backends/sandbox/internal/httpkit"
	"github.com/GizClaw/flowcraft/backends/sandbox/internal/httpkit/mitm"
)

// RunnerOption configures a Runner at construction time.
type RunnerOption func(*runnerConfig)

// runnerConfig is the resolved set of options shared between platforms.
// It lives in the platform-neutral file so the option functions
// type-check on every OS even though Runner itself is darwin-only.
type runnerConfig struct {
	binFrom  string   // raw value supplied to WithBinary, "" if defaulted
	writable []string // extra writable paths, resolved at construction
	decision func(httpkit.ProxyDecision)
	hooks    mitm.ProxyHooks
	roots    *x509.CertPool
}

// WithBinary overrides the sandbox-exec binary path. By default the
// Runner uses exec.LookPath("sandbox-exec"); set this for hermetic
// builds or testing doubles.
func WithBinary(path string) RunnerOption {
	return func(c *runnerConfig) {
		c.binFrom = path
	}
}

// WithWritablePaths grants write access to additional absolute paths
// beyond the built-in set (runner root and /dev/null). Use it for a
// dedicated temp directory or toolchain caches that legitimately live
// outside the workspace — e.g. GOCACHE under ~/Library/Caches — while
// keeping the rest of the machine write-denied. The system temp root is
// intentionally not writable by default: granting it wholesale would
// let one sandbox write another run's files. Paths are resolved
// (EvalSymlinks) at construction; each is emitted as an SBPL subpath
// rule.
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

// WithOutboundRoots overrides the roots used to verify the real
// target's TLS certificate during MITM (nil means system roots). Use
// it for custom/internal CAs; it never weakens the child side, which
// still trusts only the injected temporary CA plus system roots.
func WithOutboundRoots(roots *x509.CertPool) RunnerOption {
	return func(c *runnerConfig) {
		c.roots = roots
	}
}
