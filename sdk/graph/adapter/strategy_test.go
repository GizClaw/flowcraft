package adapter

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/compiler"
	"github.com/GizClaw/flowcraft/sdk/graph/executor"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/node/scriptnode"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// scriptFactory builds a fresh node.Factory wired with the script runtime
// for each adapter test case.
func scriptFactory() *node.Factory {
	f := node.NewFactory()
	scriptnode.Register(f, scriptnode.Deps{ScriptRuntime: jsrt.New()})
	return f
}

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
	fac := scriptFactory()
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
	fac := scriptFactory()
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

func TestGraphRunnable_ResolvesVariableReferences(t *testing.T) {
	def := &graph.GraphDefinition{
		Name:  "var-resolve",
		Entry: "s1",
		Nodes: []graph.NodeDefinition{
			{
				ID:   "s1",
				Type: "script",
				Config: map[string]any{
					"source":   `board.setVar("resolved_greeting", config.greeting);`,
					"greeting": "${board.greeting}",
				},
			},
		},
	}

	cg, err := compiler.NewCompiler().Compile(def)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	deps := workflow.NewDependencies()
	fac := scriptFactory()
	workflow.SetDep(deps, DepNodeFactory, fac)

	s := FromCompiled(cg)
	runnable, err := s.Build(context.Background(), deps)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	board := workflow.NewBoard()
	board.SetVar("greeting", "hello from board")

	result, err := runnable.Execute(context.Background(), board, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got := result.GetVarString("resolved_greeting")
	if got != "hello from board" {
		t.Fatalf("expected resolved_greeting=%q, got %q (variable reference was not resolved)", "hello from board", got)
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
	fac := scriptFactory()
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
