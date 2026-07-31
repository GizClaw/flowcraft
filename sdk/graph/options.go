package graph

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// BuildOption customises how [Build] assembles a [Graph].
type BuildOption func(*buildOptions)

type buildOptions struct {
	maxIterations  int
	timeout        time.Duration
	parallel       ParallelConfig
	maxNodeRetries int
}

// defaultMaxIterations is the built-in loop guard: a run may invoke at
// most this many nodes before Execute fails with a validation error.
const defaultMaxIterations = 100

func defaultBuildOptions() buildOptions {
	return buildOptions{maxIterations: defaultMaxIterations}
}

// WithMaxIterations caps the total number of node invocations per run
// — the loop guard for cyclic graphs. Values <= 0 keep the default.
func WithMaxIterations(n int) BuildOption {
	return func(o *buildOptions) {
		if n > 0 {
			o.maxIterations = n
		}
	}
}

// WithTimeout bounds the wall-clock duration of a single Execute call.
// Zero means no engine-level timeout (the caller's context rules).
func WithTimeout(d time.Duration) BuildOption {
	return func(o *buildOptions) { o.timeout = d }
}

// WithParallel configures concurrent execution of independent frontier
// nodes. See [ParallelConfig] for the isolation and merge model.
func WithParallel(cfg ParallelConfig) BuildOption {
	return func(o *buildOptions) { o.parallel = cfg }
}

// WithMaxNodeRetries sets how many times a failing node handler is
// retried before the run fails. Interrupted, aborted, budget-exceeded
// and validation errors are never retried. Zero means no retries.
func WithMaxNodeRetries(n int) BuildOption {
	return func(o *buildOptions) { o.maxNodeRetries = n }
}

func (o *buildOptions) validate() error {
	if o.timeout < 0 {
		return errdefs.Validationf("graph: timeout must be >= 0")
	}
	if o.maxNodeRetries < 0 {
		return errdefs.Validationf("graph: max node retries must be >= 0")
	}
	return o.parallel.validate()
}
