package nsjail

// RunnerOption configures a Runner at construction time.
type RunnerOption func(*runnerConfig)

// runnerConfig is the resolved set of options shared between platforms.
// It lives in the platform-neutral file so the option functions
// type-check on every OS even though Runner itself is Linux-only.
type runnerConfig struct {
	binFrom string   // raw value supplied to WithBinary, "" if defaulted
	extra   []string // extra nsjail flags injected before the "--" separator
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
// sparingly: per-policy flags are owned by sandbox.ExecOptions, and
// values passed here are NOT subject to the same validation as
// ExecOptions translation. This option is an escape hatch for nsjail
// features (e.g. bind mounts, seccomp policy files) that the
// sandbox.ExecOptions vocabulary does not yet cover.
func WithExtraFlags(flags ...string) RunnerOption {
	return func(c *runnerConfig) {
		c.extra = append(c.extra, flags...)
	}
}
