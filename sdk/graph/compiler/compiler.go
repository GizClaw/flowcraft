// Package compiler provides the GraphCompiler that transforms a GraphDefinition
// into a CompiledGraph with static analysis.
package compiler

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
)

// ValidateGraphDef validates a GraphDefinition.
//
// Deprecated: Use def.Validate() directly. This wrapper exists only for
// backward compatibility with existing callers. Removed in v0.2.0.
func ValidateGraphDef(d *graph.GraphDefinition) error {
	return d.Validate()
}

// CompiledGraph is the result of compilation, containing the raw graph structure
// and analysis metadata. Nodes in RawGraph are passthrough placeholders; actual
// node construction is deferred to downstream Assemble + NodeFactory.
type CompiledGraph struct {
	Graph    *graph.RawGraph
	NodeDefs []graph.NodeDefinition
	EdgeDefs []graph.EdgeDefinition
	Warnings []Warning
	Metadata graph.GraphMeta
}

// Warning represents a non-fatal issue found during compilation.
type Warning struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	NodeIDs []string `json:"node_ids,omitempty"`
}

// BuildOptions configures how the compiler processes a graph definition.
type BuildOptions struct{}

// CompilerOption configures the Compiler.
type CompilerOption func(*Compiler)

// Compiler transforms GraphDefinition into a CompiledGraph with static analysis.
// It is purely static — no runtime node instances are constructed.
type Compiler struct{}

// NewCompiler creates a new Compiler with the given options.
func NewCompiler(opts ...CompilerOption) *Compiler {
	c := &Compiler{}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Compile transforms a GraphDefinition into a CompiledGraph. All nodes are
// represented as passthrough placeholders; the original definitions are
// preserved in NodeDefs/EdgeDefs for downstream NodeFactory construction.
func (c *Compiler) Compile(def *graph.GraphDefinition, opts ...BuildOptions) (*CompiledGraph, error) {
	if err := def.Validate(); err != nil {
		return nil, err
	}

	nodes := make(map[string]graph.Node, len(def.Nodes))
	for _, nd := range def.Nodes {
		nodes[nd.ID] = graph.NewPassthroughNode(nd.ID, nd.Type)
	}

	edges := make(map[string][]graph.Edge, len(def.Edges))
	reverse := make(map[string][]string)
	for _, ed := range def.Edges {
		var cond *graph.CompiledCondition
		if ed.Condition != "" {
			var err error
			cond, err = graph.CompileCondition(ed.Condition)
			if err != nil {
				return nil, err
			}
		}
		edge := graph.Edge{From: ed.From, To: ed.To, Condition: cond}
		edges[ed.From] = append(edges[ed.From], edge)
		reverse[ed.To] = append(reverse[ed.To], ed.From)
	}

	skipConditions := make(map[string]*graph.CompiledCondition)
	for _, nd := range def.Nodes {
		if nd.SkipCondition != "" {
			compiled, err := graph.CompileCondition(nd.SkipCondition)
			if err != nil {
				return nil, errdefs.Validation(fmt.Errorf(
					"invalid skip_condition for node %q: %w", nd.ID, err))
			}
			skipConditions[nd.ID] = compiled
		}
	}

	g := &graph.RawGraph{
		Name:           def.Name,
		Entry:          def.Entry,
		Nodes:          nodes,
		Edges:          edges,
		Reverse:        reverse,
		SkipConditions: skipConditions,
	}

	warnings := analyze(g, def)

	meta := graph.GraphMeta{
		NodeCount:   len(nodes),
		EdgeCount:   len(def.Edges),
		HasCycles:   detectCycles(g),
		HasParallel: detectParallel(g),
		MaxDepth:    computeMaxDepth(g),
	}

	nodeDefs := make([]graph.NodeDefinition, len(def.Nodes))
	copy(nodeDefs, def.Nodes)
	edgeDefs := make([]graph.EdgeDefinition, len(def.Edges))
	copy(edgeDefs, def.Edges)

	return &CompiledGraph{
		Graph:    g,
		NodeDefs: nodeDefs,
		EdgeDefs: edgeDefs,
		Warnings: warnings,
		Metadata: meta,
	}, nil
}

// detectParallel checks if any node has multiple unconditional outgoing edges.
func detectParallel(g *graph.RawGraph) bool {
	for _, edges := range g.Edges {
		uncond := 0
		for _, e := range edges {
			if e.Condition == nil {
				uncond++
			}
		}
		if uncond > 1 {
			return true
		}
	}
	return false
}

// computeMaxDepth computes the longest acyclic path from entry to END using DFS.
func computeMaxDepth(g *graph.RawGraph) int {
	if g.Entry == "" {
		return 0
	}
	visited := make(map[string]bool)
	return dfsMaxDepth(g, g.Entry, visited)
}

func dfsMaxDepth(g *graph.RawGraph, nodeID string, visited map[string]bool) int {
	if nodeID == graph.END {
		return 0
	}
	if visited[nodeID] {
		return 0
	}
	visited[nodeID] = true
	defer func() { visited[nodeID] = false }()

	maxChild := -1
	for _, e := range g.Edges[nodeID] {
		d := dfsMaxDepth(g, e.To, visited)
		if d > maxChild {
			maxChild = d
		}
	}
	if maxChild < 0 {
		return 0
	}
	return 1 + maxChild
}

// detectCycles uses DFS coloring to check if the graph has any cycles.
// Implementation delegates to findCycleNodes for code reuse.
func detectCycles(g *graph.RawGraph) bool {
	return len(findCycleNodes(g)) > 0
}

// successors builds a successor map from edge data.
func successors(g *graph.RawGraph) map[string][]string {
	succs := make(map[string][]string)
	for from, edges := range g.Edges {
		for _, e := range edges {
			succs[from] = append(succs[from], e.To)
		}
	}
	return succs
}

// FormatWarnings returns a human-readable string of all warnings.
func FormatWarnings(ws []Warning) string {
	if len(ws) == 0 {
		return ""
	}
	msg := fmt.Sprintf("%d warning(s):\n", len(ws))
	for _, w := range ws {
		msg += fmt.Sprintf("  [%s] %s", w.Code, w.Message)
		if len(w.NodeIDs) > 0 {
			msg += fmt.Sprintf(" (nodes: %v)", w.NodeIDs)
		}
		msg += "\n"
	}
	return msg
}
