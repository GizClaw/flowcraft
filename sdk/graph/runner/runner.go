package runner

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/event"
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
	executor executor.Executor //nolint:staticcheck // the deprecated interface is the executor's own internal contract; runner keeps using it until v0.3.0 inlines the loop
	host     engine.Host

	// runOpts collects executor.RunOption values produced by
	// runner.WithMaxIterations / WithTimeout / WithParallel / …
	// They are appended to the per-execution option list inside
	// Execute, in declaration order, so behaviour matches calling
	// the underlying executor.WithXxx directly today.
	runOpts []executor.RunOption //nolint:staticcheck // executor.RunOption is itself the soon-to-go interface; runner shields callers from that.
}

// Option configures a Runner.
type Option func(*Runner)

// WithExecutor overrides the default LocalExecutor.
//
// Deprecated: scheduled for removal in v0.3.0 together with
// [executor.Executor]. The graph engine will be exposed exclusively
// through [Runner] which itself implements [engine.Engine]; hosting
// alternative execution backends will be done by writing a fresh
// [engine.Engine] implementation rather than swapping a sub-interface.
func WithExecutor(e executor.Executor) Option { //nolint:staticcheck
	return func(r *Runner) { r.executor = e }
}

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

// WithEventBus sets the Bus used for graph lifecycle events.
//
// Deprecated: pass an engine.Host via WithHost — the Runner now publishes
// every envelope through host.Publish. WithEventBus is retained as a
// transitional shim that wraps the bus in a minimal host (other Host
// methods become no-ops); it will be removed in v0.3.0 alongside
// executor.WithEventBus.
func WithEventBus(bus event.Bus) Option {
	return func(r *Runner) {
		if bus == nil {
			bus = event.NoopBus{}
		}
		r.host = busOnlyHost{Host: engine.NoopHost{}, bus: bus}
	}
}

// busOnlyHost adapts an event.Bus into engine.Host. It exists only to keep
// the deprecated WithEventBus working without polluting Runner with a
// second event-sink field. Every Host method other than Publish is
// inherited from engine.NoopHost via the embedded field, so callers that
// only care about lifecycle envelopes still get the right behaviour while
// nodes that try to call Interrupt/AskUser/etc. see a safe default.
//
// Deprecated: scheduled for removal in v0.3.0 together with
// runner.WithEventBus.
type busOnlyHost struct {
	engine.Host // embeds engine.NoopHost in practice; Publish is overridden below.
	bus         event.Bus
}

// Publish forwards to the wrapped bus, swallowing errors to match the
// "events are observability, not control flow" rule the executor relies on.
func (h busOnlyHost) Publish(ctx context.Context, env event.Envelope) error {
	if h.bus == nil {
		return nil
	}
	_ = h.bus.Publish(ctx, env)
	return nil
}

// unwrapBus returns the underlying bus when h originated from
// runner.WithEventBus. Used by the deprecated Runner.Bus() getter so
// existing callers keep working until v0.3.0.
func (h busOnlyHost) unwrapBus() event.Bus { return h.bus }

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
		executor: executor.NewLocalExecutor(),
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
func (r *Runner) Run(ctx context.Context, vars map[string]any, opts ...executor.RunOption) (*graph.Board, error) { //nolint:staticcheck
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
// Resume support: the graph runner does not implement run.ResumeFrom
// today. Supplying a non-nil ResumeFrom yields an
// errdefs.NotAvailable-classified error (see [Runner.Execute]'s
// implementation in engine.go).
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
	extra []executor.RunOption, //nolint:staticcheck
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

	// Reject foreign / unsupported resume up front so the executor
	// never sees the parameter and stays a pure single-shot engine.
	// This also makes the error class deterministic for callers — the
	// engine contract requires Validation for foreign ExecID and
	// NotAvailable for "resume not implemented", and we match both.
	if run.ResumeFrom != nil {
		if err := classifyResume(run); err != nil {
			return board, err
		}
	}

	opts := make([]executor.RunOption, 0, 3+len(r.runOpts)+len(extra)) //nolint:staticcheck
	opts = append(opts, executor.WithHost(host))
	if run.ID != "" {
		opts = append(opts, executor.WithRunID(run.ID)) //nolint:staticcheck
	}
	// Default resolver is harmless if the caller already supplied one
	// via runner.WithResolver — executor.runConfig is last-write-wins.
	opts = append(opts, executor.WithResolver(variable.NewResolver()))
	opts = append(opts, r.runOpts...)
	opts = append(opts, extra...)

	return r.executor.Execute(ctx, g, board, opts...)
}

// Host returns the configured engine.Host. Always non-nil — callers can
// invoke any Host method directly (Publish / Interrupts / AskUser /
// Checkpoint / ReportUsage). Subscribing to envelopes is the host
// implementation's concern; if the concrete type exposes a getter for
// that, callers can type-assert on the returned value.
func (r *Runner) Host() engine.Host { return r.host }

// Bus returns the bus configured via the deprecated WithEventBus option,
// or nil if WithHost was used (the modern path) or no option was supplied.
//
// Deprecated: prefer Runner.Host() and any host-specific getters your host
// implementation exposes. Scheduled for removal in v0.3.0.
func (r *Runner) Bus() event.Bus {
	type busUnwrapper interface{ unwrapBus() event.Bus }
	if u, ok := r.host.(busUnwrapper); ok {
		return u.unwrapBus()
	}
	return nil
}

// Graph returns a freshly assembled Graph snapshot for inspection. Intended
// for testing and debugging, not for execution.
func (r *Runner) Graph() (*graph.Graph, error) {
	return Assemble(r.compiled, r.factory)
}
