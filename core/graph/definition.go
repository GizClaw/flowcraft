package graph

import (
	"encoding/json"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// END is the sentinel edge target marking graph termination. An edge
// whose To is END exits the run when taken; it does not refer to a
// real node.
const END = "__end__"

// GraphDefinition is the serialisable declarative definition of a
// graph (the wire layer).
//
// It is deliberately JSON-shaped at the wire layer. A graph is not a
// shared resource: [Build] returns a *Graph that IS an agent.Engine,
// so declarative authoring enters through the engine settings of a
// deployment document (sdkx/deploy) rather than a config package of
// its own. sdk/graph/config accepts YAML as authoring sugar and
// converts it to JSON with sdk/config/utils before this type is
// decoded; the kernel itself never parses YAML, which keeps node
// configs in a single canonical encoding.
//
// Validate performs structural checks (unique ids, known edge
// endpoints, entry presence). Semantic checks — unknown node types,
// config field names, condition syntax, I/O role resolution — happen
// in [Build], which has access to the [Registry].
type GraphDefinition struct {
	// Name identifies the graph and appears in error messages. When
	// Version is empty, Name is also recorded in checkpoints as the
	// spec version marker.
	Name string `json:"name"`

	// Version optionally marks the graph's spec version. It is
	// recorded in checkpoints and compared on resume; bump it when
	// node semantics change so old checkpoints are not silently
	// replayed against a different definition. Empty means "no
	// explicit version" and falls back to Name.
	Version string `json:"version,omitempty"`

	// Entry is the id of the node where a fresh run starts.
	Entry string `json:"entry"`

	// Nodes lists all nodes. Ids must be unique; Entry must be one of
	// them.
	Nodes []NodeDefinition `json:"nodes"`

	// Edges lists directed transitions. From must name a node; To
	// names a node or END.
	Edges []EdgeDefinition `json:"edges"`
}

// NodeDefinition describes a single node in a [GraphDefinition].
type NodeDefinition struct {
	// ID uniquely names the node within the graph.
	ID string `json:"id"`

	// Type selects the registered node type ([Registry]) supplying the
	// behaviour.
	Type string `json:"type"`

	// Config is the node's opaque configuration as raw JSON. The
	// kernel never interprets it beyond two well-defined points:
	//
	//   - at [Build] time, top-level field names are checked against
	//     the node type's config struct (unknown fields are rejected);
	//   - at execution time, "${board.<name>}" references inside
	//     string values are resolved against the live board before the
	//     typed decode runs (see package doc, layer 4).
	//
	// Everything else — defaults, required fields, semantics — is the
	// node type's own [NodeType.Decode] contract.
	Config json.RawMessage `json:"config,omitempty"`

	// SkipCondition is an optional expr-lang expression over board
	// vars (same syntax as edge conditions). When it evaluates to true
	// the node's handler is not invoked, but the node still routes:
	// its outgoing edges are followed as if it had run.
	SkipCondition string `json:"skip_condition,omitempty"`
}

// EdgeDefinition describes a single directed transition.
type EdgeDefinition struct {
	// From is the source node id.
	From string `json:"from"`

	// To is the target node id, or END to terminate the run.
	To string `json:"to"`

	// Condition is an optional expr-lang expression evaluated against
	// board vars after the source node completes. The edge is taken
	// only when the expression is absent or evaluates to true.
	// Multiple outgoing edges may fire (fan-out); zero firing edges
	// ends the branch.
	//
	// The environment also exposes VarIterations ("__iterations"), the
	// node invocation count, so a loop back-edge can soft-exit with
	// e.g. "__iterations < 10" instead of tripping WithMaxIterations.
	Condition string `json:"condition,omitempty"`
}

// Validate checks that the definition is structurally sound.
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
		if n.Type == "" {
			return errdefs.Validationf("node %q: type is required", n.ID)
		}
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
