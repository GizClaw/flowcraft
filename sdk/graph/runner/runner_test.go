package runner_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/engine/enginetest"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/runner"
)

// --- test helpers ---

type testNode struct {
	id     string
	typ    string
	execFn func(ctx graph.ExecutionContext, board *graph.Board) error
}

func (n *testNode) ID() string   { return n.id }
func (n *testNode) Type() string { return n.typ }
func (n *testNode) ExecuteBoard(ctx graph.ExecutionContext, board *graph.Board) error {
	if n.execFn != nil {
		return n.execFn(ctx, board)
	}
	return nil
}

func newTestNode(id string, fn func(graph.ExecutionContext, *graph.Board) error) *testNode {
	return &testNode{id: id, typ: "test", execFn: fn}
}

func testFactory(builders map[string]node.NodeBuilder) *node.Factory {
	f := node.NewFactory()
	for typ, b := range builders {
		f.RegisterBuilder(typ, b)
	}
	return f
}

func testNodeBuilder(fn func(graph.ExecutionContext, *graph.Board) error) node.NodeBuilder {
	return func(def graph.NodeDefinition) (graph.Node, error) {
		return newTestNode(def.ID, fn), nil
	}
}

// --- tests ---

func TestRunner_SimpleGraph(t *testing.T) {
	def := &graph.GraphDefinition{
		Name:  "simple",
		Entry: "start",
		Nodes: []graph.NodeDefinition{
			{ID: "start", Type: "passthrough"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "start", To: graph.END},
		},
	}

	r, err := runner.New(def, node.NewFactory())
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	result, err := r.Run(context.Background(), map[string]any{"query": "hello"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v := result.GetVarString("query"); v != "hello" {
		t.Fatalf("expected 'hello', got %q", v)
	}
}

func TestRunner_TwoNodePipeline(t *testing.T) {
	factory := testFactory(map[string]node.NodeBuilder{
		"set_a": testNodeBuilder(func(_ graph.ExecutionContext, b *graph.Board) error {
			b.SetVar("step", "a_done")
			return nil
		}),
		"set_b": testNodeBuilder(func(_ graph.ExecutionContext, b *graph.Board) error {
			b.SetVar("step", "b_done")
			return nil
		}),
	})

	def := &graph.GraphDefinition{
		Name:  "pipeline",
		Entry: "a",
		Nodes: []graph.NodeDefinition{
			{ID: "a", Type: "set_a"},
			{ID: "b", Type: "set_b"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "a", To: "b"},
			{From: "b", To: graph.END},
		},
	}

	r, err := runner.New(def, factory)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	result, err := r.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v := result.GetVarString("step"); v != "b_done" {
		t.Fatalf("expected 'b_done', got %q", v)
	}
}

func TestRunner_ConditionalRouting(t *testing.T) {
	factory := testFactory(map[string]node.NodeBuilder{
		"yes": testNodeBuilder(func(_ graph.ExecutionContext, b *graph.Board) error {
			b.SetVar("branch", "yes")
			return nil
		}),
		"no": testNodeBuilder(func(_ graph.ExecutionContext, b *graph.Board) error {
			b.SetVar("branch", "no")
			return nil
		}),
	})

	def := &graph.GraphDefinition{
		Name:  "cond",
		Entry: "start",
		Nodes: []graph.NodeDefinition{
			{ID: "start", Type: "passthrough"},
			{ID: "yes_node", Type: "yes"},
			{ID: "no_node", Type: "no"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "start", To: "yes_node", Condition: "approved == true"},
			{From: "start", To: "no_node"},
			{From: "yes_node", To: graph.END},
			{From: "no_node", To: graph.END},
		},
	}

	r, err := runner.New(def, factory)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	result, err := r.Run(context.Background(), map[string]any{"approved": true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v := result.GetVarString("branch"); v != "yes" {
		t.Fatalf("expected 'yes', got %q", v)
	}
}

func TestRunner_InvalidDefinition(t *testing.T) {
	def := &graph.GraphDefinition{
		Name:  "",
		Entry: "",
	}
	_, err := runner.New(def, node.NewFactory())
	if err == nil {
		t.Fatal("expected error for invalid definition")
	}
}

func TestRunner_ConcurrentSafety(t *testing.T) {
	var counter atomic.Int64
	factory := testFactory(map[string]node.NodeBuilder{
		"inc": testNodeBuilder(func(_ graph.ExecutionContext, b *graph.Board) error {
			n := counter.Add(1)
			b.SetVar("count", n)
			return nil
		}),
	})

	def := &graph.GraphDefinition{
		Name:  "concurrent",
		Entry: "inc_node",
		Nodes: []graph.NodeDefinition{
			{ID: "inc_node", Type: "inc"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "inc_node", To: graph.END},
		},
	}

	r, err := runner.New(def, factory)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Run(context.Background(), map[string]any{"x": 1})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent Run failed: %v", err)
	}

	if n := counter.Load(); n != goroutines {
		t.Fatalf("expected %d executions, got %d", goroutines, n)
	}
}

// TestRunner_WithHost confirms graph lifecycle envelopes are routed through
// engine.Host.Publish (the v0.3 path) when the user supplies WithHost. The
// MockHost lets us assert on every envelope without standing up an event
// bus, mirroring the way agent/agent.go drives executors today.
func TestRunner_WithHost(t *testing.T) {
	host := enginetest.NewMockHost()

	def := &graph.GraphDefinition{
		Name:  "host_test",
		Entry: "start",
		Nodes: []graph.NodeDefinition{
			{ID: "start", Type: "passthrough"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "start", To: graph.END},
		},
	}

	r, err := runner.New(def, node.NewFactory(), runner.WithHost(host))
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	if r.Host() != host {
		t.Fatal("Host() should return the configured host")
	}

	const runID = "rh-1"
	if _, err := r.Execute(context.Background(),
		engine.Run{ID: runID}, host, engine.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	envs := host.Envelopes()
	if len(envs) == 0 {
		t.Fatal("expected host to receive envelopes")
	}
	wantPrefix := string(engine.SubjectPrefix) + runID + "."
	sawStart, sawEnd := false, false
	for _, env := range envs {
		s := string(env.Subject)
		if !strings.HasPrefix(s, wantPrefix) {
			t.Fatalf("unexpected subject %q (want prefix %q)", s, wantPrefix)
		}
		if strings.HasSuffix(s, ".start") {
			sawStart = true
		}
		if strings.HasSuffix(s, ".end") {
			sawEnd = true
		}
	}
	if !sawStart || !sawEnd {
		t.Fatalf("missing lifecycle events (sawStart=%v sawEnd=%v)", sawStart, sawEnd)
	}
}

func TestRunner_Graph(t *testing.T) {
	def := &graph.GraphDefinition{
		Name:  "inspect",
		Entry: "start",
		Nodes: []graph.NodeDefinition{
			{ID: "start", Type: "passthrough"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "start", To: graph.END},
		},
	}

	r, err := runner.New(def, node.NewFactory())
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	g, err := r.Graph()
	if err != nil {
		t.Fatalf("Graph: %v", err)
	}
	if g.Name() != "inspect" {
		t.Fatalf("expected graph name 'inspect', got %q", g.Name())
	}
	if g.Entry() != "start" {
		t.Fatalf("expected entry 'start', got %q", g.Entry())
	}
}
