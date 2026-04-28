package runner

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/compiler"
	"github.com/GizClaw/flowcraft/sdk/graph/executor"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/variable"
)

// Runner is a lightweight, concurrency-safe graph executor.
//
// It caches the CompiledGraph (static analysis result) and re-assembles fresh
// Node instances on every Run call, so concurrent callers never share mutable
// node state.
//
// Internally the Runner only knows about engine.Host. Callers who still pass
// the deprecated WithEventBus see their bus folded into a thin host adapter
// (busOnlyHost) at construction time, so there is exactly one event sink to
// reason about from Run() onwards.
type Runner struct {
	compiled *compiler.CompiledGraph
	factory  *node.Factory
	executor executor.Executor
	host     engine.Host
}

// Option configures a Runner.
type Option func(*Runner)

// WithExecutor overrides the default LocalExecutor.
func WithExecutor(e executor.Executor) Option {
	return func(r *Runner) { r.executor = e }
}

// WithHost installs the engine.Host the Runner forwards to the executor on
// every Run. The host receives every published envelope and is also handed
// to nodes via ExecutionContext.Host so they can call Publish, Interrupt,
// AskUser etc. directly.
//
// When omitted the Runner defaults to engine.NoopHost{} and envelopes are
// dropped.
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
	compiled, err := compiler.NewCompiler().Compile(def)
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

// Run assembles a fresh graph, populates a Board from vars, and executes.
// Safe for concurrent use — each call gets independent node instances.
func (r *Runner) Run(ctx context.Context, vars map[string]any, opts ...executor.RunOption) (*graph.Board, error) {
	g, err := Assemble(r.compiled, r.factory)
	if err != nil {
		return nil, err
	}

	board := graph.NewBoard()
	for k, v := range vars {
		board.SetVar(k, v)
	}

	merged := []executor.RunOption{
		executor.WithHost(r.host),
		executor.WithResolver(variable.NewResolver()),
	}
	merged = append(merged, opts...)

	return r.executor.Execute(ctx, g, board, merged...)
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
