package executor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
)

func testFactory(builders map[string]node.NodeBuilder) *node.Factory {
	f := node.NewFactory()
	for typ, b := range builders {
		f.RegisterBuilder(typ, b)
	}
	return f
}

func testNodeBuilder(fn func(graph.ExecutionContext, *graph.Board) error) node.NodeBuilder {
	return func(def graph.NodeDefinition, _ *node.BuildContext) (graph.Node, error) {
		return newTestNode(def.ID, fn), nil
	}
}

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

	runner, err := NewRunner(def, node.NewFactory())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	result, err := runner.Run(context.Background(), map[string]any{"query": "hello"})
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

	runner, err := NewRunner(def, factory)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	result, err := runner.Run(context.Background(), nil)
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

	runner, err := NewRunner(def, factory)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	result, err := runner.Run(context.Background(), map[string]any{"approved": true})
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
	_, err := NewRunner(def, node.NewFactory())
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

	runner, err := NewRunner(def, factory)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := runner.Run(context.Background(), map[string]any{"x": 1})
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

func TestRunner_WithEventBus(t *testing.T) {
	bus := event.NewMemoryBus()
	defer func() { _ = bus.Close() }()

	def := &graph.GraphDefinition{
		Name:  "bus_test",
		Entry: "start",
		Nodes: []graph.NodeDefinition{
			{ID: "start", Type: "passthrough"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "start", To: graph.END},
		},
	}

	runner, err := NewRunner(def, node.NewFactory(), WithRunnerEventBus(bus))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	if runner.Bus() != bus {
		t.Fatal("Bus() should return the configured bus")
	}

	sub, _ := bus.Subscribe(context.Background(), event.EventFilter{
		Types: []event.EventType{event.EventGraphStart, event.EventGraphEnd},
	})

	_, err = runner.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := 0
	for got < 2 {
		select {
		case <-sub.Events():
			got++
		default:
			t.Fatalf("expected 2 events (start+end), got %d", got)
		}
	}
}

func TestRunner_StreamCallback(t *testing.T) {
	factory := testFactory(map[string]node.NodeBuilder{
		"emitter": testNodeBuilder(func(ctx graph.ExecutionContext, b *graph.Board) error {
			if ctx.Stream != nil {
				ctx.Stream(graph.StreamEvent{Type: "token", NodeID: "emit", Payload: map[string]any{"content": "hi"}})
			}
			b.SetVar("done", true)
			return nil
		}),
	})

	def := &graph.GraphDefinition{
		Name:  "stream",
		Entry: "emit",
		Nodes: []graph.NodeDefinition{
			{ID: "emit", Type: "emitter"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "emit", To: graph.END},
		},
	}

	runner, err := NewRunner(def, factory)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	var captured []graph.StreamEvent
	_, err = runner.Run(context.Background(), nil, WithStreamCallback(func(se graph.StreamEvent) {
		captured = append(captured, se)
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("expected 1 stream event, got %d", len(captured))
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

	runner, err := NewRunner(def, node.NewFactory())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	g, err := runner.Graph()
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
