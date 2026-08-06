package nsjail

// RunnerOption configures a Runner at construction time.
type RunnerOption func(*runnerConfig)

// runnerConfig is the resolved set of options shared between platforms.
// It lives in the platform-neutral file so the option functions
// type-check on every OS even though Runner itself is Linux-only.
type runnerConfig struct {
	binFrom  string   // raw value supplied to WithBinary, "" if defaulted
	extra    []string // extra nsjail flags injected before the "--" separator
	writable []string // additional explicitly writable host paths
}

// WithBinary overrides the nsjail binary path. By default the Runner
// uses exec.LookPath("nsjail"); set this for hermetic builds where
// nsjail lives in a vendored directory.
func WithBinary(path string) RunnerOption {
	return func(c *runnerConfig) {
		c.binFrom = path
	}
}

// WithExtraFlags injects extra arguments between the auto-generated
// flag list and the "--" separator that precedes the command. Use
// sparingly: per-policy flags are owned by sandbox.ExecOptions.
//
// Flags that can weaken the mount boundary (--rw, --chroot, mount /
// bind-mount options, or --disable_clone_newns) are rejected by New.
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
