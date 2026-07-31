package seatbelt

// RunnerOption configures a Runner at construction time.
type RunnerOption func(*runnerConfig)

// runnerConfig is the resolved set of options shared between platforms.
// It lives in the platform-neutral file so the option functions
// type-check on every OS even though Runner itself is darwin-only.
type runnerConfig struct {
	binFrom  string   // raw value supplied to WithBinary, "" if defaulted
	writable []string // extra writable paths, resolved at construction
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
