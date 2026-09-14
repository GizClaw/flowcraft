package graph

import (
	"time"

	"github.com/GizClaw/flowcraft/core/agent/bindings"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// BuildOption customises how [Build] assembles a [Graph].
type BuildOption func(*buildOptions)

type buildOptions struct {
	maxIterations        int
	timeout              time.Duration
	runEndPublishTimeout time.Duration
	parallel             ParallelConfig
	maxNodeRetries       int
	scriptBindings       bindings.Provider
}

const (
	// defaultMaxIterations is the built-in loop guard: a run may route at
	// most this many nodes before Execute fails with a budget error.
	// WithMaxIterations(0) lifts the guard for graphs known to terminate.
	defaultMaxIterations        = 100
	defaultRunEndPublishTimeout = 5 * time.Second
)

func defaultBuildOptions() buildOptions {
	return buildOptions{
		maxIterations:        defaultMaxIterations,
		runEndPublishTimeout: defaultRunEndPublishTimeout,
	}
}

// WithMaxIterations caps how many nodes one run may route — the loop
// guard for cyclic graphs.
//
//   - n > 0 caps the run at n routed nodes. A skipped node still
//     routes (execution continues along its outgoing edges), so it
//     consumes budget: a cycle whose nodes are all skipped still
//     terminates instead of spinning forever.
//   - n == 0 lifts the guard entirely. The run is bounded only by its
//     context and by whatever exit conditions the definition carries,
//     so only use it for graphs known to terminate.
//   - n < 0 fails [Build] with a validation error.
//
// Omitting the option keeps [defaultMaxIterations].
func WithMaxIterations(n int) BuildOption {
	return func(o *buildOptions) { o.maxIterations = n }
}

// WithTimeout bounds the wall-clock duration of a single Execute call.
// Zero means no engine-level timeout (the caller's context rules).
func WithTimeout(d time.Duration) BuildOption {
	return func(o *buildOptions) { o.timeout = d }
}

// WithRunEndPublishTimeout bounds the best-effort terminal event publish.
// It must be positive. The default is five seconds, allowing normal network
// and subscriber backpressure while still preventing an unbounded Execute.
func WithRunEndPublishTimeout(d time.Duration) BuildOption {
	return func(o *buildOptions) { o.runEndPublishTimeout = d }
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

// WithScriptBindings sets the engine-level script bindings provider:
// one provider serves every script node of the built graph — the
// built-in "script" type and script-backed custom node types alike —
// and it replaces (rather than extends) the standard bindings those
// node types would otherwise assemble from their own deps.
//
// The graph engine factory wires this from the optional
// "script_bindings" deployment dep; library callers building a graph
// directly can set it themselves. Omitting the option keeps the
// standard script surface.
func WithScriptBindings(provider bindings.Provider) BuildOption {
	return func(o *buildOptions) { o.scriptBindings = provider }
}

func (o *buildOptions) validate() error {
	if o.maxIterations < 0 {
		return errdefs.Validationf("graph: max iterations must be >= 0")
	}
	if o.timeout < 0 {
		return errdefs.Validationf("graph: timeout must be >= 0")
	}
	if o.runEndPublishTimeout <= 0 {
		return errdefs.Validationf("graph: run-end publish timeout must be > 0")
	}
	if o.maxNodeRetries < 0 {
		return errdefs.Validationf("graph: max node retries must be >= 0")
	}
	return o.parallel.validate()
}
