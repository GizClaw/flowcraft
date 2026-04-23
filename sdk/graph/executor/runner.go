package executor

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/compiler"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/variable"
)

// Runner is a lightweight, concurrency-safe graph executor.
//
// It caches the CompiledGraph (static analysis result) and re-assembles
// fresh Node instances on every Run call, so concurrent callers never
// share mutable node state.
type Runner struct {
	compiled *compiler.CompiledGraph
	factory  *node.Factory
	executor Executor
	bus      event.LegacyEventBus
}

// RunnerOption configures a Runner.
type RunnerOption func(*Runner)

// WithRunnerExecutor overrides the default LocalExecutor.
func WithRunnerExecutor(e Executor) RunnerOption {
	return func(r *Runner) { r.executor = e }
}

// WithRunnerEventBus sets the LegacyEventBus used for graph lifecycle events.
// Defaults to event.LegacyNoopBus{}.
func WithRunnerEventBus(bus event.LegacyEventBus) RunnerOption {
	return func(r *Runner) { r.bus = bus }
}

// NewRunner compiles a GraphDefinition and returns a ready-to-use Runner.
// The factory provides runtime dependencies (LLM resolver, tool registry, etc.)
// needed to instantiate nodes.
func NewRunner(def *graph.GraphDefinition, factory *node.Factory, opts ...RunnerOption) (*Runner, error) {
	compiled, err := compiler.NewCompiler().Compile(def)
	if err != nil {
		return nil, err
	}
	r := &Runner{
		compiled: compiled,
		factory:  factory,
		executor: NewLocalExecutor(),
		bus:      event.LegacyNoopBus{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// Run assembles a fresh graph, populates a Board from vars, and executes.
// Safe for concurrent use — each call gets independent node instances.
func (r *Runner) Run(ctx context.Context, vars map[string]any, opts ...RunOption) (*graph.Board, error) {
	g, err := Assemble(r.compiled, r.factory)
	if err != nil {
		return nil, err
	}

	board := graph.NewBoard()
	for k, v := range vars {
		board.SetVar(k, v)
	}

	merged := []RunOption{
		WithEventBus(r.bus),
		WithResolver(variable.NewResolver()),
	}
	merged = append(merged, opts...)

	return r.executor.Execute(ctx, g, board, merged...)
}

// Bus returns the configured LegacyEventBus for external subscription.
func (r *Runner) Bus() event.LegacyEventBus { return r.bus }

// Graph returns a freshly assembled Graph snapshot for inspection.
// Intended for testing and debugging, not for execution.
func (r *Runner) Graph() (*graph.Graph, error) {
	return Assemble(r.compiled, r.factory)
}
