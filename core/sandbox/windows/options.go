package windows

import "math"

// Option configures a Runner at construction time.
type Option func(*runnerConfig)

// runnerConfig is the resolved set of options. It lives in the
// platform-neutral file so the option functions type-check on every
// OS even though Runner itself is Windows-only.
type runnerConfig struct {
	defaultMaxOutput int64
	writeConfine     bool
	writable         []string // extra writable paths (write confinement)
}

// WithMaxOutputBytes sets the default per-call MaxOutputBytes used
// when sandbox.ExecOptions.Resources.MaxOutputBytes is zero. Pass a
// non-positive value to disable truncation (i.e. allow up to
// math.MaxInt64 bytes).
func WithMaxOutputBytes(n int64) Option {
	return func(c *runnerConfig) {
		if n <= 0 {
			c.defaultMaxOutput = math.MaxInt64
		} else {
			c.defaultMaxOutput = n
		}
	}
}

// WithWriteConfinement enables OS-level write confinement: every child
// runs with a restricted, Low-integrity token, and the runner root
// (plus any WithWritablePaths entries) is labeled Low so the child can
// write only there. Everything else on the host stays Medium:
// readable, not writable. The child gets its own Low-labeled TEMP/TMP.
//
// Confinement is opt-in: without it the backend is the phase-1
// lifecycle-only sandbox and WriteReadOnly is rejected with
// NotAvailable.
func WithWriteConfinement() Option {
	return func(c *runnerConfig) {
		c.writeConfine = true
	}
}

// WithWritablePaths grants write access to additional existing paths
// beyond rootDir when write confinement is enabled. Explicit writable
// paths remain writable even for a WriteReadOnly call, mirroring the
// bwrap / seatbelt semantics. Paths are resolved to absolute form at
// construction.
func WithWritablePaths(paths ...string) Option {
	return func(c *runnerConfig) {
		c.writable = append(c.writable, paths...)
	}
}
