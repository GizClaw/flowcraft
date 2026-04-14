package adapter

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/compiler"
	"github.com/GizClaw/flowcraft/sdk/graph/executor"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
	"github.com/GizClaw/flowcraft/sdk/workflow"

	_ "github.com/GizClaw/flowcraft/sdk/graph/node/scriptnode"
)

func TestFromDefinition_BuildRequiresFactory(t *testing.T) {
	s := FromDefinition(&graph.GraphDefinition{Name: "t", Entry: "x"})
	_, err := s.Build(context.Background(), workflow.NewDependencies())
	if err == nil {
		t.Fatal("expected error without factory")
	}
}

func TestFromDefinition_Kind(t *testing.T) {
	s := FromDefinition(&graph.GraphDefinition{Name: "t", Entry: "x"})
	if s.Kind() != "graph" {
		t.Fatalf("Kind()=%q, want graph", s.Kind())
	}
}

func TestFromDefinition_Capabilities(t *testing.T) {
	s := FromDefinition(&graph.GraphDefinition{Name: "t", Entry: "x"})
	if s.Capabilities().AnswerVar() != workflow.VarAnswer {
		t.Fatalf("AnswerVar()=%q", s.Capabilities().AnswerVar())
	}
}

func TestFromDefinition_NilDefinition(t *testing.T) {
	s := FromDefinition(nil)
	_, err := s.Build(context.Background(), workflow.NewDependencies())
	if err == nil {
		t.Fatal("expected error for nil definition")
	}
}

func TestFromDefinition_CompileErrorNotCached(t *testing.T) {
	s := FromDefinition(&graph.GraphDefinition{Name: "bad"})

	_, err1 := s.Build(context.Background(), workflow.NewDependencies())
	if err1 == nil {
		t.Fatal("first build should fail")
	}

	_, err2 := s.Build(context.Background(), workflow.NewDependencies())
	if err2 == nil {
		t.Fatal("second build should also fail (retried, not cached)")
	}
}

func TestFromCompiled_NilCompiledGraph(t *testing.T) {
	s := FromCompiled(nil)
	if s.Kind() != "graph" {
		t.Fatalf("Kind()=%q", s.Kind())
	}
	_, err := s.Build(context.Background(), workflow.NewDependencies())
	if err == nil {
		t.Fatal("expected error for nil compiled graph")
	}
}

func TestFromCompiled_Capabilities(t *testing.T) {
	s := FromCompiled(nil)
	if s.Capabilities().AnswerVar() != workflow.VarAnswer {
		t.Fatalf("AnswerVar()=%q", s.Capabilities().AnswerVar())
	}
}

func TestNewGraphRunnable_WithFactory(t *testing.T) {
	def := &graph.GraphDefinition{
		Name:  "test",
		Entry: "start",
		Nodes: []graph.NodeDefinition{
			{ID: "start", Type: "answer"},
		},
	}

	cg, err := compiler.NewCompiler().Compile(def)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	deps := workflow.NewDependencies()
	fac := node.NewFactory(node.WithScriptRuntime(jsrt.New()))
	workflow.SetDep(deps, DepNodeFactory, fac)

	s := FromCompiled(cg)
	runnable, err := s.Build(context.Background(), deps)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if runnable == nil {
		t.Fatal("expected non-nil runnable")
	}
}

func TestNewGraphRunnable_WithCustomExecutor(t *testing.T) {
	def := &graph.GraphDefinition{
		Name:  "test",
		Entry: "start",
		Nodes: []graph.NodeDefinition{
			{ID: "start", Type: "answer"},
		},
	}

	cg, err := compiler.NewCompiler().Compile(def)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	deps := workflow.NewDependencies()
	fac := node.NewFactory(node.WithScriptRuntime(jsrt.New()))
	workflow.SetDep(deps, DepNodeFactory, fac)
	workflow.SetDep(deps, DepExecutor, executor.NewLocalExecutor())

	s := FromCompiled(cg)
	runnable, err := s.Build(context.Background(), deps)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if runnable == nil {
		t.Fatal("expected non-nil runnable")
	}
}

func TestFromDefinition_CompiledCacheHit(t *testing.T) {
	def := &graph.GraphDefinition{
		Name:  "cache-test",
		Entry: "start",
		Nodes: []graph.NodeDefinition{
			{ID: "start", Type: "answer"},
		},
	}

	s := FromDefinition(def)

	deps := workflow.NewDependencies()
	fac := node.NewFactory(node.WithScriptRuntime(jsrt.New()))
	workflow.SetDep(deps, DepNodeFactory, fac)

	r1, err := s.Build(context.Background(), deps)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}

	r2, err := s.Build(context.Background(), deps)
	if err != nil {
		t.Fatalf("second build: %v", err)
	}

	if r1 == nil || r2 == nil {
		t.Fatal("expected non-nil runnables")
	}
}
