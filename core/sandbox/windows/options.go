package windows

import "math"

// Option configures a Runner at construction time.
type Option func(*runnerConfig)

// runnerConfig is the resolved set of options. It lives in the
// platform-neutral file so the option functions type-check on every
// OS even though Runner itself is Windows-only.
type runnerConfig struct {
	defaultMaxOutput int64
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
