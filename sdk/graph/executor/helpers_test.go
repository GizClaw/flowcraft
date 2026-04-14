package executor

import "github.com/GizClaw/flowcraft/sdk/graph"

// testNode is a configurable test node that executes a callback.
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

// configurableTestNode implements graph.Configurable for variable resolution tests.
type configurableTestNode struct {
	id     string
	config map[string]any
	execFn func(graph.ExecutionContext, *graph.Board, map[string]any) error
}

func (n *configurableTestNode) ID() string                 { return n.id }
func (n *configurableTestNode) Type() string               { return "test" }
func (n *configurableTestNode) Config() map[string]any     { return n.config }
func (n *configurableTestNode) SetConfig(c map[string]any) { n.config = c }
func (n *configurableTestNode) ExecuteBoard(ctx graph.ExecutionContext, board *graph.Board) error {
	if n.execFn != nil {
		return n.execFn(ctx, board, n.config)
	}
	return nil
}

// buildGraph creates an executable Graph from raw components for testing.
func buildGraph(name, entry string, nodes map[string]graph.Node, edges []graph.Edge) *graph.Graph {
	edgeMap := make(map[string][]graph.Edge)
	reverse := make(map[string][]string)
	for _, e := range edges {
		edgeMap[e.From] = append(edgeMap[e.From], e)
		reverse[e.To] = append(reverse[e.To], e.From)
	}
	return graph.NewGraph(&graph.RawGraph{
		Name:           name,
		Entry:          entry,
		Nodes:          nodes,
		Edges:          edgeMap,
		Reverse:        reverse,
		SkipConditions: make(map[string]*graph.CompiledCondition),
	}, graph.GraphMeta{})
}

func copyMap(m map[string]any) map[string]any {
	c := make(map[string]any, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}
