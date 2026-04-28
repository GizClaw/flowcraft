package graph

import (
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// GraphDefinition is the declarative definition of a graph (YAML/JSON).
type GraphDefinition struct {
	Name  string           `json:"name" yaml:"name"`
	Entry string           `json:"entry" yaml:"entry"`
	Nodes []NodeDefinition `json:"nodes" yaml:"nodes"`
	Edges []EdgeDefinition `json:"edges" yaml:"edges"`
}

// Validate checks that the definition is structurally valid.
func (d *GraphDefinition) Validate() error {
	if d.Name == "" {
		return errdefs.Validationf("graph name is required")
	}
	if d.Entry == "" {
		return errdefs.Validationf("graph entry node is required")
	}
	if len(d.Nodes) == 0 {
		return errdefs.Validationf("graph must have at least one node")
	}

	nodeIDs := make(map[string]bool, len(d.Nodes))
	for _, n := range d.Nodes {
		if n.ID == "" {
			return errdefs.Validationf("node ID is required")
		}
		if nodeIDs[n.ID] {
			return errdefs.Validationf("duplicate node ID %q", n.ID)
		}
		nodeIDs[n.ID] = true
	}

	if !nodeIDs[d.Entry] {
		return errdefs.Validationf("entry node %q not found in nodes", d.Entry)
	}

	for _, e := range d.Edges {
		if !nodeIDs[e.From] {
			return errdefs.Validationf("edge from unknown node %q", e.From)
		}
		if e.To != END && !nodeIDs[e.To] {
			return errdefs.Validationf("edge to unknown node %q", e.To)
		}
	}
	return nil
}

// NodeDefinition describes a single node in a GraphDefinition.
type NodeDefinition struct {
	ID            string         `json:"id" yaml:"id"`
	Type          string         `json:"type" yaml:"type"`
	Config        map[string]any `json:"config,omitempty" yaml:"config,omitempty"`
	SkipCondition string         `json:"skip_condition,omitempty" yaml:"skip_condition,omitempty"`
}

// EdgeDefinition describes a single edge in a GraphDefinition.
type EdgeDefinition struct {
	From      string `json:"from" yaml:"from"`
	To        string `json:"to" yaml:"to"`
	Condition string `json:"condition,omitempty" yaml:"condition,omitempty"`
}

// RawGraph is the intermediate graph structure produced by the compiler.
//
// All fields are exported for static analysis during compilation. It is NOT
// intended for direct execution — use Assemble (graph/runner) to produce an
// immutable *Graph for the executor.
type RawGraph struct {
	Name           string
	Entry          string
	Nodes          map[string]Node
	Edges          map[string][]Edge
	Reverse        map[string][]string
	SkipConditions map[string]*CompiledCondition
}

// GraphMeta contains structural analysis results produced by the compiler.
type GraphMeta struct {
	NodeCount   int  `json:"node_count"`
	EdgeCount   int  `json:"edge_count"`
	HasCycles   bool `json:"has_cycles"`
	HasParallel bool `json:"has_parallel"`
	MaxDepth    int  `json:"max_depth"`
}
