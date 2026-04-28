package adapter

import (
	"context"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/runner"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// ExtExecutorRunOpts is the Request.Extensions key for additional
// runner-side options. The value MUST be []runner.Option (Runner
// construction options); per-Run knobs that used to be
// []executor.RunOption no longer have an equivalent — runner.Runner
// owns them at construction time now.
//
// Deprecated: scheduled for removal in v0.3.0 together with the rest
// of this adapter. Construct a runner.Runner directly and call
// agent.Run with it instead.
const ExtExecutorRunOpts = "adapter.executor_run_opts"

// Dependency keys for SetDep / GetDep.
const (
	// DepNodeFactory is the workflow.Dependencies key holding a
	// *node.Factory the adapter will use to assemble runnables.
	DepNodeFactory = "node.factory"

	// DepExecutor used to override the underlying executor
	// implementation. With the v0.2 → v0.3 internalisation of the
	// executor, the only execution backend is graph/runner.Runner;
	// the dependency is read for backwards-compatibility but its
	// value is now ignored.
	//
	// Deprecated: scheduled for removal in v0.3.0.
	DepExecutor = "executor"
)

// FromDefinition returns a graph Strategy that compiles def (cached) and
// executes via graph/runner.Runner.
func FromDefinition(def *graph.GraphDefinition) workflow.Strategy {
	return &graphStrategy{def: def}
}

// FromCompiled returns a Strategy that reuses a pre-compiled graph
// (e.g. from a process-wide cache).
func FromCompiled(cg *graph.CompiledGraph) workflow.Strategy {
	return &compiledStrategy{cg: cg}
}

type graphStrategy struct {
	def *graph.GraphDefinition
	mu  sync.Mutex
	cg  *graph.CompiledGraph
}

func (s *graphStrategy) Kind() string { return "graph" }

func (s *graphStrategy) Capabilities() workflow.StrategyCapabilities {
	return workflow.StrategyCapabilities{AnswerKey: workflow.VarAnswer}
}

func (s *graphStrategy) compiled() (*graph.CompiledGraph, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cg != nil {
		return s.cg, nil
	}
	if s.def == nil {
		return nil, fmt.Errorf("adapter: nil graph definition")
	}
	cg, err := graph.Compile(s.def)
	if err != nil {
		return nil, err
	}
	s.cg = cg
	return s.cg, nil
}

func (s *graphStrategy) Build(ctx context.Context, deps *workflow.Dependencies) (workflow.Runnable, error) {
	cg, err := s.compiled()
	if err != nil {
		return nil, err
	}
	return newGraphRunnable(cg, deps)
}

type compiledStrategy struct {
	cg *graph.CompiledGraph
}

func (s *compiledStrategy) Kind() string { return "graph" }

func (s *compiledStrategy) Capabilities() workflow.StrategyCapabilities {
	return workflow.StrategyCapabilities{AnswerKey: workflow.VarAnswer}
}

func (s *compiledStrategy) Build(ctx context.Context, deps *workflow.Dependencies) (workflow.Runnable, error) {
	if s.cg == nil {
		return nil, fmt.Errorf("adapter: nil compiled graph")
	}
	return newGraphRunnable(s.cg, deps)
}

// graphRunnable wraps a graph definition + factory in a workflow.Runnable.
// It owns no executor of its own; instead it delegates each Execute to a
// fresh runner.Runner that itself encapsulates the (now internal) execution
// loop. This keeps the adapter on the same execution path as new agent.Run
// callers, so behaviour stays identical between the workflow and agent
// surfaces during the v0.2 → v0.3 transition.
type graphRunnable struct {
	def     *graph.GraphDefinition
	cg      *graph.CompiledGraph
	factory *node.Factory
}

func newGraphRunnable(cg *graph.CompiledGraph, deps *workflow.Dependencies) (*graphRunnable, error) {
	fac, err := workflow.GetDep[*node.Factory](deps, DepNodeFactory)
	if err != nil {
		return nil, fmt.Errorf("adapter: %w", err)
	}
	return &graphRunnable{cg: cg, factory: fac}, nil
}

func (r *graphRunnable) Execute(ctx context.Context, board *workflow.Board, req *workflow.Request, opts ...workflow.RunOption) (*workflow.Board, error) {
	// Recover the original GraphDefinition from the cached CompiledGraph
	// so we can re-feed it to runner.New. The CompiledGraph carries the
	// raw definition pieces (NodeDefs, EdgeDefs, Name, Entry) — enough
	// for runner.New to recompile and produce a fresh executor instance.
	def := &graph.GraphDefinition{
		Name:  r.cg.Graph.Name,
		Entry: r.cg.Graph.Entry,
		Nodes: r.cg.NodeDefs,
		Edges: r.cg.EdgeDefs,
	}

	rOpts := []runner.Option{}
	if req != nil && req.Extensions != nil {
		if raw, ok := req.Extensions[ExtExecutorRunOpts]; ok {
			if sl, ok := raw.([]runner.Option); ok {
				rOpts = append(rOpts, sl...)
			}
		}
	}
	rc := workflow.ApplyRunOpts(opts)
	if rc.StreamCallback != nil {
		rOpts = append(rOpts, runner.WithStreamCallback(rc.StreamCallback))
	}
	if rc.MaxIterations > 0 {
		rOpts = append(rOpts, runner.WithMaxIterations(rc.MaxIterations))
	}

	rn, err := runner.New(def, r.factory, rOpts...)
	if err != nil {
		return board, err
	}

	gboard := graphBoardFromWorkflow(board)
	var run engine.Run
	if req != nil {
		run.ID = req.RunID
	}
	out, runErr := rn.Execute(ctx, run, nil, gboard)
	return workflowBoardFromGraph(out), runErr
}

// graphBoardFromWorkflow projects a *workflow.Board into a *graph.Board via
// snapshot round-trip. Used by adapter to bridge the two now-independent
// blackboard types until workflow is removed in v0.3.0.
func graphBoardFromWorkflow(b *workflow.Board) *graph.Board {
	if b == nil {
		return graph.NewBoard()
	}
	wsnap := b.Snapshot()
	if wsnap == nil {
		return graph.NewBoard()
	}
	return graph.RestoreBoard(&graph.BoardSnapshot{
		Vars:     wsnap.Vars,
		Channels: wsnap.Channels,
	})
}

// workflowBoardFromGraph is the inverse of graphBoardFromWorkflow.
func workflowBoardFromGraph(b *graph.Board) *workflow.Board {
	if b == nil {
		return workflow.NewBoard()
	}
	gsnap := b.Snapshot()
	if gsnap == nil {
		return workflow.NewBoard()
	}
	return workflow.RestoreBoard(&workflow.BoardSnapshot{
		Vars:     gsnap.Vars,
		Channels: gsnap.Channels,
	})
}
