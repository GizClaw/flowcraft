package graph

// Static-analysis compiler for GraphDefinition.
//
// Compile turns a declarative GraphDefinition into a CompiledGraph
// that the runner can assemble into a runnable Graph. The compiler is
// purely static: it constructs no node instances, performs no I/O, and
// has no runtime dependencies. This makes it cheap to call ahead of
// time (CI lint, validation hooks, dashboards) and lets callers cache
// the CompiledGraph across many runs.
//
// Historically this code lived in sdk/graph/compiler. It moved into
// the graph package proper because every compiler exported symbol
// names a concept owned by graph (RawGraph, NodeDefinition,
// CompiledCondition) and the indirection added no isolation —
// "compile a graph" reads more naturally as a graph.Compile call.

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// CompiledGraph is the result of compilation, containing the raw graph
// structure and analysis metadata. Nodes in RawGraph are passthrough
// placeholders; actual node construction is deferred to downstream
// Assemble + NodeFactory.
type CompiledGraph struct {
	Graph    *RawGraph
	NodeDefs []NodeDefinition
	EdgeDefs []EdgeDefinition
	Warnings []Warning
	Metadata GraphMeta
}

// Warning represents a non-fatal issue found during compilation.
type Warning struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	NodeIDs []string `json:"node_ids,omitempty"`
}

// Compile transforms a GraphDefinition into a CompiledGraph. All nodes
// are represented as passthrough placeholders; the original definitions
// are preserved in NodeDefs/EdgeDefs for downstream NodeFactory
// construction. The returned CompiledGraph is safe to cache and reuse
// across many runner.New calls — compilation has no side effects.
//
// Compile is a pure static analysis pass: it constructs no node
// instances and performs no I/O. Errors are validation failures
// (errdefs.Validation); non-fatal issues surface as Warnings on the
// result.
func Compile(def *GraphDefinition) (*CompiledGraph, error) {
	if err := def.Validate(); err != nil {
		return nil, err
	}

	nodes := make(map[string]Node, len(def.Nodes))
	for _, nd := range def.Nodes {
		nodes[nd.ID] = NewPassthroughNode(nd.ID, nd.Type)
	}

	edges := make(map[string][]Edge, len(def.Edges))
	reverse := make(map[string][]string)
	for _, ed := range def.Edges {
		var cond *CompiledCondition
		if ed.Condition != "" {
			var err error
			cond, err = CompileCondition(ed.Condition)
			if err != nil {
				return nil, err
			}
		}
		edge := Edge{From: ed.From, To: ed.To, Condition: cond}
		edges[ed.From] = append(edges[ed.From], edge)
		reverse[ed.To] = append(reverse[ed.To], ed.From)
	}

	skipConditions := make(map[string]*CompiledCondition)
	for _, nd := range def.Nodes {
		if nd.SkipCondition != "" {
			compiled, err := CompileCondition(nd.SkipCondition)
			if err != nil {
				return nil, errdefs.Validation(fmt.Errorf(
					"invalid skip_condition for node %q: %w", nd.ID, err))
			}
			skipConditions[nd.ID] = compiled
		}
	}

	g := &RawGraph{
		Name:           def.Name,
		Entry:          def.Entry,
		Nodes:          nodes,
		Edges:          edges,
		Reverse:        reverse,
		SkipConditions: skipConditions,
	}

	warnings := analyze(g, def)

	meta := GraphMeta{
		NodeCount:   len(nodes),
		EdgeCount:   len(def.Edges),
		HasCycles:   detectCycles(g),
		HasParallel: detectParallel(g),
		MaxDepth:    computeMaxDepth(g),
	}

	nodeDefs := make([]NodeDefinition, len(def.Nodes))
	copy(nodeDefs, def.Nodes)
	edgeDefs := make([]EdgeDefinition, len(def.Edges))
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
func detectParallel(g *RawGraph) bool {
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
func computeMaxDepth(g *RawGraph) int {
	if g.Entry == "" {
		return 0
	}
	visited := make(map[string]bool)
	return dfsMaxDepth(g, g.Entry, visited)
}

func dfsMaxDepth(g *RawGraph, nodeID string, visited map[string]bool) int {
	if nodeID == END {
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
func detectCycles(g *RawGraph) bool {
	return len(findCycleNodes(g)) > 0
}

// successors builds a successor map from edge data.
func successors(g *RawGraph) map[string][]string {
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
