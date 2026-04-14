package adapter

import (
	"context"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/compiler"
	"github.com/GizClaw/flowcraft/sdk/graph/executor"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// ExtExecutorRunOpts is the Request.Extensions key for []executor.RunOption.
const ExtExecutorRunOpts = "adapter.executor_run_opts"

// Dependency keys for SetDep / GetDep.
const (
	DepNodeFactory = "node.factory"
	DepExecutor    = "executor"
)

// FromDefinition returns a graph Strategy that compiles def (cached) and executes via the local executor.
func FromDefinition(def *graph.GraphDefinition) workflow.Strategy {
	return &graphStrategy{def: def}
}

// FromCompiled returns a Strategy that reuses a pre-compiled graph (e.g. from a process-wide cache).
func FromCompiled(cg *compiler.CompiledGraph) workflow.Strategy {
	return &compiledStrategy{cg: cg}
}

type graphStrategy struct {
	def *graph.GraphDefinition
	mu  sync.Mutex
	cg  *compiler.CompiledGraph
}

func (s *graphStrategy) Kind() string { return "graph" }

func (s *graphStrategy) Capabilities() workflow.StrategyCapabilities {
	return workflow.StrategyCapabilities{AnswerKey: workflow.VarAnswer}
}

func (s *graphStrategy) compiled() (*compiler.CompiledGraph, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cg != nil {
		return s.cg, nil
	}
	if s.def == nil {
		return nil, fmt.Errorf("adapter: nil graph definition")
	}
	cg, err := compiler.NewCompiler().Compile(s.def)
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
	cg *compiler.CompiledGraph
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

type graphRunnable struct {
	cg       *compiler.CompiledGraph
	factory  *node.Factory
	executor executor.Executor
}

func newGraphRunnable(cg *compiler.CompiledGraph, deps *workflow.Dependencies) (*graphRunnable, error) {
	fac, err := workflow.GetDep[*node.Factory](deps, DepNodeFactory)
	if err != nil {
		return nil, fmt.Errorf("adapter: %w", err)
	}
	exec, _ := workflow.GetDep[executor.Executor](deps, DepExecutor)
	if exec == nil {
		exec = executor.NewLocalExecutor()
	}
	return &graphRunnable{cg: cg, factory: fac, executor: exec}, nil
}

func (r *graphRunnable) Execute(ctx context.Context, board *workflow.Board, req *workflow.Request, opts ...workflow.RunOption) (*workflow.Board, error) {
	g, err := executor.Assemble(r.cg, r.factory)
	if err != nil {
		return board, err
	}
	var execOpts []executor.RunOption
	if req != nil && req.Extensions != nil {
		if raw, ok := req.Extensions[ExtExecutorRunOpts]; ok {
			if sl, ok := raw.([]executor.RunOption); ok {
				execOpts = append(execOpts, sl...)
			}
		}
	}
	rc := workflow.ApplyRunOpts(opts)
	if rc.StreamCallback != nil {
		execOpts = append(execOpts, executor.WithStreamCallback(rc.StreamCallback))
	}
	if rc.MaxIterations > 0 {
		execOpts = append(execOpts, executor.WithMaxIterations(rc.MaxIterations))
	}
	return r.executor.Execute(ctx, g, board, execOpts...)
}
