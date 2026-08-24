package windows

// RunnerOption configures a Runner at construction time.
type RunnerOption func(*runnerConfig)

// runnerConfig is the resolved set of options shared between
// platforms. It lives in the platform-neutral file so the option
// functions type-check on every OS even though Runner itself is
// windows-only.
type runnerConfig struct {
	writable            []string // additional explicitly writable host paths
	defaultMaxOutput    int64    // value applied when setDefaultMaxOutput
	setDefaultMaxOutput bool     // distinguishes "unset" from "explicit non-positive"
	level               string   // restricted-token (P1) or elevated (P2)
}

// WithWritablePaths grants write access to additional existing paths
// beyond rootDir. On Windows this means the paths receive allow-write
// ACEs for the workspace capability SID during the P1 fill-in; the
// rest of the filesystem stays readable, write-denied by default.
// Use this for dedicated build caches; do not grant shared roots such
// as the whole user profile wholesale.
func WithWritablePaths(paths ...string) RunnerOption {
	return func(c *runnerConfig) {
		c.writable = append(c.writable, paths...)
	}
}

// WithMaxOutputBytes sets the default per-call MaxOutputBytes used
// when sandbox.ExecOptions.Resources.MaxOutputBytes is zero. Pass a
// non-positive value to disable truncation.
func WithMaxOutputBytes(n int64) RunnerOption {
	return func(c *runnerConfig) {
		c.defaultMaxOutput = n
		c.setDefaultMaxOutput = true
	}
}

// WithLevel selects the enforcement backend: LevelRestrictedToken
// (unelevated, P1) or LevelElevated (dedicated account + elevated
// helper, P2).
func WithLevel(level string) RunnerOption {
	return func(c *runnerConfig) {
		c.level = level
	}
}
