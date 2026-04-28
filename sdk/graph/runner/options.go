package runner

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/runner/internal/executor"
)

// This file collects the construction-time configuration that the Runner
// applies to every Execute / Run call. The split is deliberate:
//
//   - Options that describe *how this graph executes* (iteration limits,
//     timeouts, parallel policy, variable resolver, …) belong here. They
//     live for the lifetime of the Runner and are mirrored as
//     executor.RunOption when the Runner dispatches an execution.
//
//   - Options that describe *one specific run* (engine.Run.ID, host, …)
//     are passed through engine.Engine.Execute or runner.Run, NOT here.
//
// This is the v0.3.0 shape: when executor.RunOption is removed, callers
// only need runner.WithXxx — there is no double-track API to migrate.
// The executor package's own WithXxx remain for in-tree tests until then.

// --- forwarders to executor.RunOption ----------------------------------------
//
// These thin wrappers exist so callers depend only on the runner package
// going forward. Each forwarder names the underlying configuration
// concept and stores a closure that the Runner replays as part of every
// execution. The trailing executor option ordering matches the
// declaration order in executor.runConfig so behaviour is identical to
// passing the option directly today.

// WithMaxIterations caps the number of executor iterations (one
// iteration ≈ one node frontier).
func WithMaxIterations(n int) Option {
	return appendRunOpt(executor.WithMaxIterations(n))
}

// WithMaxNodeRetries caps the per-node retry budget the executor applies
// when a node returns a transient error.
func WithMaxNodeRetries(n int) Option {
	return appendRunOpt(executor.WithMaxNodeRetries(n))
}

// WithTimeout sets a wall-clock budget for the whole run; the executor
// derives a context.WithTimeout for its main loop.
func WithTimeout(d time.Duration) Option {
	return appendRunOpt(executor.WithTimeout(d))
}

// WithStartNode overrides the entry node for the run. Useful for resume
// flows that want to restart from a specific point.
func WithStartNode(id string) Option {
	return appendRunOpt(executor.WithStartNode(id))
}

// WithParallel enables parallel fork/join execution with the supplied
// policy. Defaults are filled in by executor.WithParallel.
func WithParallel(cfg executor.ParallelConfig) Option {
	return appendRunOpt(executor.WithParallel(cfg))
}

// WithResolver installs the variable resolver consulted by the executor
// when a node config contains references. The Runner installs a
// fresh variable.NewResolver() per execution when this option is
// omitted (see runner.go).
func WithResolver(r executor.VariableResolver) Option {
	return appendRunOpt(executor.WithResolver(r))
}

// --- deprecated forwarders (kept for v0.2 callers) ---------------------------

// WithStreamCallback installs a legacy node-delta stream callback.
//
// Deprecated: subscribe to engine.Host's event bus, or read
// ExecutionContext.Publisher inside a node, instead. Scheduled for
// removal in v0.3.0 together with executor.WithStreamCallback.
func WithStreamCallback(cb graph.StreamCallback) Option {
	return appendRunOpt(executor.WithStreamCallback(cb))
}

// WithCheckpointStore installs a graph-format CheckpointStore that
// persists a checkpoint after every node completes.
//
// Deprecated: prefer WithHost with a host whose Checkpointer wraps an
// engine.CheckpointStore. Scheduled for removal in v0.3.0 together with
// executor.WithCheckpointStore.
func WithCheckpointStore(s executor.CheckpointStore) Option { //nolint:staticcheck // forwards to deprecated executor option for transitional callers
	return appendRunOpt(executor.WithCheckpointStore(s))
}

// appendRunOpt is the single seam that grows Runner.runOpts so all
// forwarders share one append site. Keeping this private avoids
// callers reaching into runOpts directly.
func appendRunOpt(o executor.RunOption) Option {
	return func(r *Runner) {
		r.runOpts = append(r.runOpts, o)
	}
}
