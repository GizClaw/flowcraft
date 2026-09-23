package seatbelt

import (
	"crypto/x509"

	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/journal"
	"github.com/GizClaw/flowcraft/core/utils/net"
)

// RunnerOption configures a Runner at construction time.
type RunnerOption func(*runnerConfig)

// runnerConfig is the resolved set of options shared between platforms.
// It lives in the platform-neutral file so the option functions
// type-check on every OS even though Runner itself is darwin-only.
type runnerConfig struct {
	binFrom      string   // raw value supplied to WithBinary, "" if defaulted
	writable     []string // extra writable paths, resolved at construction
	readOnlyRoot bool     // keep the runner root read-only for every exec
	journal      *journal.Config
	decision     func(net.ProxyDecision)
	hooks        net.MITMHooks
	roots        *x509.CertPool
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

// WithFileJournal attaches a file journal to the runner: every write
// under the runner root and under the explicitly writable paths becomes
// a readable event (see the core/sandbox/journal package).
//
// The journal watches host-side paths, which is exactly what the
// Seatbelt profile exposes to the sandbox: the root and the writable
// paths are opened at their own absolute paths, so a write inside the
// sandbox is a write to the same host path. Events under the root are
// reported relative to it; events under a writable path outside the
// root carry that absolute path.
//
// The journal is an observation stream, not a boundary — the write
// confinement the profile enforces decides what is allowed, and the
// journal reports what happened. On this platform the watch costs one
// descriptor per watched entry rather than per directory, which is what
// [sandbox.JournalCapabilities.WatchBudget] counts.
func WithFileJournal(opts sandbox.JournalOptions) RunnerOption {
	return WithFileJournalConfig(journal.Config{Options: opts})
}

// WithFileJournalConfig is [WithFileJournal] for a caller that already
// resolved a [journal.Config] — the resource factory, which validates
// the deployment settings before the runner exists.
func WithFileJournalConfig(cfg journal.Config) RunnerOption {
	return func(c *runnerConfig) {
		requested := cfg
		c.journal = &requested
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
