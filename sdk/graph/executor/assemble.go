package executor

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/compiler"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
)

// Assemble takes a CompiledGraph (static analysis result) and a Factory,
// then constructs all real node instances and returns an immutable executable Graph.
func Assemble(compiled *compiler.CompiledGraph, factory *node.Factory) (*graph.Graph, error) {
	if compiled == nil {
		return nil, errdefs.Validationf("compiled graph is nil")
	}

	nodes := make(map[string]graph.Node, len(compiled.NodeDefs))
	for _, nd := range compiled.NodeDefs {
		n, err := factory.Build(nd)
		if err != nil {
			return nil, errdefs.Validation(fmt.Errorf(
				"failed to build node %q (type %q): %w", nd.ID, nd.Type, err))
		}
		nodes[nd.ID] = n
	}

	raw := &graph.RawGraph{
		Name:           compiled.Graph.Name,
		Entry:          compiled.Graph.Entry,
		Nodes:          nodes,
		Edges:          compiled.Graph.Edges,
		Reverse:        compiled.Graph.Reverse,
		SkipConditions: compiled.Graph.SkipConditions,
	}

	return graph.NewGraph(raw, compiled.Metadata), nil
}
