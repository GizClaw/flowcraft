package runner

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/runner/internal/executor"
	"github.com/GizClaw/flowcraft/sdk/graph/variable"
)

// Runner is a graph engine: it caches the CompiledGraph (static analysis
// result) and re-assembles fresh Node instances on every execution, so
// concurrent callers never share mutable node state.
//
// Runner satisfies engine.Engine via [Runner.Execute], which is the
// canonical entry point going forward — agent.Run accepts a Runner
// directly, no adapter required. [Runner.Run] remains as a convenience
// helper for callers who want to drive the graph without standing up an
// engine.Run + engine.Host pair (typical for tests or one-shot CLI
// usage); it is implemented as a thin wrapper around Execute so there
// is exactly one execution path to reason about.
//
// Runner construction collects every graph-level configuration knob
// (max iterations, timeout, parallel policy, variable resolver, …)
// once via runner.WithXxx options. Per-run identity (engine.Run.ID)
// and host capabilities (engine.Host) flow through Execute parameters
// — they are NOT construction-time concerns.
type Runner struct {
	compiled *graph.CompiledGraph
	factory  *node.Factory
	host     engine.Host

	// runOpts collects executor.RunOption values produced by
	// runner.WithMaxIterations / WithTimeout / WithParallel / …
	// They are appended to the per-execution option list inside
	// Execute, in declaration order, so behaviour matches calling
	// the underlying executor.WithXxx directly today.
	runOpts []executor.RunOption
}

// Option configures a Runner.
type Option func(*Runner)

// WithHost installs the engine.Host the Runner forwards to the executor
// on every Run. The host receives every published envelope and is also
// handed to nodes via ExecutionContext.Host so they can call Publish,
// Interrupt, AskUser etc. directly.
//
// When omitted the Runner defaults to engine.NoopHost{} and envelopes
// are dropped. Note that [Runner.Execute] takes a Host parameter that
// overrides this default — WithHost only matters for [Runner.Run]
// callers.
func WithHost(h engine.Host) Option {
	return func(r *Runner) {
		if h == nil {
			h = engine.NoopHost{}
		}
		r.host = h
	}
}

// New compiles a GraphDefinition and returns a ready-to-use Runner. The
// factory provides runtime dependencies (LLM resolver, tool registry, etc.)
// needed to instantiate nodes.
func New(def *graph.GraphDefinition, factory *node.Factory, opts ...Option) (*Runner, error) {
	compiled, err := graph.Compile(def)
	if err != nil {
		return nil, err
	}
	r := &Runner{
		compiled: compiled,
		factory:  factory,
		host:     engine.NoopHost{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// Run is a convenience wrapper around [Runner.Execute] for callers who
// want to drive the graph without assembling an engine.Run + engine.Host
// pair themselves. It populates a fresh Board from vars, mints an
// engine.Run that carries any extra executor.RunOption supplied via
// opts, and forwards the Runner's configured host. Safe for concurrent
// use — each call gets independent node instances.
//
// New code that runs through agent.Run should call agent.Run with the
// Runner directly; Run is preserved for tests and one-shot CLI usage
// where the engine.Engine plumbing would be ceremony.
func (r *Runner) Run(ctx context.Context, vars map[string]any, opts ...executor.RunOption) (*graph.Board, error) {
	board := graph.NewBoard()
	for k, v := range vars {
		board.SetVar(k, v)
	}

	// extraOpts piggy-backs on the per-call slot in executeBound so the
	// caller's options layer over the Runner's construction-time options
	// in the same order they did before this refactor.
	return r.executeBound(ctx, engine.Run{}, r.host, board, opts)
}

// Execute satisfies [engine.Engine]. It runs the bound graph using
// host as the event/interrupt/ask sink and run.ID as the executor's
// run identifier. board MUST be non-nil; engines mutate in place by
// contract and Execute therefore returns the same pointer on success.
//
// Resume support: when run.ResumeFrom is non-nil the runner restores
// board state from cp.Board, locates the node id recorded in cp.Step,
// and continues from that node's downstream edges instead of
// re-executing it. The checkpoint MUST originate from a previous run
// of the same graph and run.ID; mismatched ExecID surfaces
// errdefs.Validation, mismatched GraphName surfaces errdefs.Validation
// via the [Resumer.CanResume] probe.
func (r *Runner) Execute(
	ctx context.Context,
	run engine.Run,
	host engine.Host,
	board *engine.Board,
) (*engine.Board, error) {
	return r.executeBound(ctx, run, host, board, nil)
}

// executeBound is the single execution seam used by both Run and
// Execute. Centralising the option assembly here keeps "what gets
// passed to the underlying executor" easy to audit and ensures any
// future ExecuteOption surface is added in exactly one place.
//
// extra is the per-call slot used by [Runner.Run] callers that still
// want to pass executor.RunOption directly; it is nil from Execute
// (engine.Engine.Execute has no equivalent slot — and intentionally so).
func (r *Runner) executeBound(
	ctx context.Context,
	run engine.Run,
	host engine.Host,
	board *graph.Board,
	extra []executor.RunOption,
) (*graph.Board, error) {
	if host == nil {
		host = r.host
	}
	if host == nil {
		host = engine.NoopHost{}
	}

	g, err := Assemble(r.compiled, r.factory)
	if err != nil {
		return board, err
	}
	if board == nil {
		board = graph.NewBoard()
	}

	// Validate resume preconditions before handing the checkpoint
	// to the executor. validateResume enforces the engine.Engine
	// resume contract (foreign ExecID is a Validation error) plus
	// the graph-runner-specific GraphName check; the executor then
	// trusts the checkpoint and only handles the executor-local
	// work of restoring board state and locating downstream nodes.
	if run.ResumeFrom != nil {
		if err := r.validateResume(*run.ResumeFrom, run.ID); err != nil {
			return board, err
		}
	}

	opts := make([]executor.RunOption, 0, 4+len(r.runOpts)+len(extra))
	opts = append(opts, executor.WithHost(host))
	if run.ID != "" {
		opts = append(opts, executor.WithRunID(run.ID))
	}
	// Default resolver is harmless if the caller already supplied one
	// via runner.WithResolver — executor.runConfig is last-write-wins.
	opts = append(opts, executor.WithResolver(variable.NewResolver()))
	if run.ResumeFrom != nil {
		opts = append(opts, executor.WithResumeFrom(run.ResumeFrom))
	}
	opts = append(opts, r.runOpts...)
	opts = append(opts, extra...)

	return executor.NewLocalExecutor().Execute(ctx, g, board, opts...)
}

// Host returns the configured engine.Host. Always non-nil — callers can
// invoke any Host method directly (Publish / Interrupts / AskUser /
// Checkpoint / ReportUsage). Subscribing to envelopes is the host
// implementation's concern; if the concrete type exposes a getter for
// that, callers can type-assert on the returned value.
func (r *Runner) Host() engine.Host { return r.host }

// Graph returns a freshly assembled Graph snapshot for inspection. Intended
// for testing and debugging, not for execution.
func (r *Runner) Graph() (*graph.Graph, error) {
	return Assemble(r.compiled, r.factory)
}
