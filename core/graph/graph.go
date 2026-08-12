package graph

import (
	"encoding/json"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Graph is a compiled, immutable graph — an [agent.Engine] ready to
// execute.
//
// Build it once via [Build] and run it concurrently any number of
// times: all per-run state lives on the board passed to Execute, never
// on the Graph.
type Graph struct {
	name        string
	specVersion string
	entry       string

	nodes map[string]*nodeSlot
	order []string // definition order, for deterministic waves
	edges map[string][]Edge

	maxIterations        int
	timeout              time.Duration
	runEndPublishTimeout time.Duration
	parallel             ParallelConfig
	maxNodeRetries       int

	warnings []Warning
}

// nodeSlot is one node's assembled runtime form: its definition, the
// resolved behaviour (registered type or fallback), compiled skip
// condition, and statically resolved I/O roles.
type nodeSlot struct {
	def NodeDefinition

	// decode/invoke come from the registered type. fallback is
	// non-nil instead when the type is unregistered and the registry
	// provided a [FallbackHandler].
	decode   func(json.RawMessage) (any, error)
	invoke   func(ExecutionContext, *agent.Board, any) error
	fallback FallbackHandler

	skipCondition *CompiledCondition
	reads         []resolvedRole
	writes        []resolvedRole
}

// Edge is a compiled transition out of a node.
type Edge struct {
	From string
	To   string // a node id, or END

	// Condition is nil for unconditional edges.
	Condition *CompiledCondition
}

// Stats summarises a built graph for diagnostics and tests.
type Stats struct {
	Nodes           int
	Edges           int
	NodeTypes       int
	MaxIterations   int
	ParallelEnabled bool
}

// Build validates def against reg and assembles an executable [Graph].
//
// Beyond [GraphDefinition.Validate]'s structural checks, Build:
//
//   - requires every node type to be registered (unless the registry
//     has a fallback);
//   - rejects unknown top-level config field names against the node
//     type's config struct;
//   - compiles edge and skip conditions (syntax errors fail here, not
//     mid-run);
//   - statically resolves every declared I/O role's board key
//     (missing required ConfigKey fields and reference-valued keys
//     fail here);
//   - collects topology [Warning]s (unreachable / dead-end nodes).
//
// Config *values* are still opaque at this point — they may contain
// "${board.*}" references that only resolve at execution time.
func Build(def *GraphDefinition, reg *Registry, opts ...BuildOption) (*Graph, error) {
	if def == nil {
		return nil, errdefs.Validationf("graph: definition is required")
	}
	if err := def.Validate(); err != nil {
		return nil, err
	}
	if reg == nil {
		return nil, errdefs.Validationf("graph: registry is required")
	}

	options := defaultBuildOptions()
	for _, opt := range opts {
		opt(&options)
	}
	if err := options.validate(); err != nil {
		return nil, err
	}

	specVersion := def.Name
	if def.Version != "" {
		specVersion = def.Version
	}

	g := &Graph{
		name:                 def.Name,
		specVersion:          specVersion,
		entry:                def.Entry,
		nodes:                make(map[string]*nodeSlot, len(def.Nodes)),
		edges:                make(map[string][]Edge, len(def.Edges)),
		maxIterations:        options.maxIterations,
		timeout:              options.timeout,
		runEndPublishTimeout: options.runEndPublishTimeout,
		parallel:             options.parallel,
		maxNodeRetries:       options.maxNodeRetries,
	}

	for _, nd := range def.Nodes {
		slot, err := buildNodeSlot(nd, reg)
		if err != nil {
			return nil, err
		}
		g.nodes[nd.ID] = slot
		g.order = append(g.order, nd.ID)
	}

	for _, ed := range def.Edges {
		e := Edge{From: ed.From, To: ed.To}
		if ed.Condition != "" {
			cond, err := compileCondition(ed.Condition)
			if err != nil {
				return nil, err
			}
			e.Condition = cond
		}
		g.edges[ed.From] = append(g.edges[ed.From], e)
	}

	g.warnings = analyzeGraph(g.entry, g.nodes, g.order, g.edges)
	return g, nil
}

// buildNodeSlot assembles one node's runtime form.
func buildNodeSlot(nd NodeDefinition, reg *Registry) (*nodeSlot, error) {
	slot := &nodeSlot{def: nd}

	et, ok := reg.types[nd.Type]
	switch {
	case !ok && reg.fallback == nil:
		return nil, errdefs.NotFoundf(
			"graph: node %q: unregistered type %q (and no fallback handler)", nd.ID, nd.Type)
	case !ok:
		slot.fallback = reg.fallback
	default:
		slot.decode = et.decode
		slot.invoke = et.invoke
		if err := checkConfigFields(nd, et); err != nil {
			return nil, err
		}
		for _, r := range et.meta.Reads {
			rr, present, err := r.resolve(nd.Type, nd.Config)
			if err != nil {
				return nil, err
			}
			if present {
				slot.reads = append(slot.reads, rr)
			}
		}
		for _, w := range et.meta.Writes {
			rr, present, err := w.resolve(nd.Type, nd.Config)
			if err != nil {
				return nil, err
			}
			if present {
				slot.writes = append(slot.writes, rr)
			}
		}
	}

	if nd.SkipCondition != "" {
		cond, err := compileCondition(nd.SkipCondition)
		if err != nil {
			return nil, err
		}
		slot.skipCondition = cond
	}
	return slot, nil
}

// checkConfigFields rejects unknown top-level config field names.
// Reference-containing *values* are fine — only the key set is
// examined, which references cannot affect.
func checkConfigFields(nd NodeDefinition, et *erasedType) error {
	if et.configFields == nil || len(nd.Config) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(nd.Config, &fields); err != nil {
		return errdefs.Validationf("graph: node %q: config is not a JSON object: %v", nd.ID, err)
	}
	for name := range fields {
		if !et.configFields[name] {
			return errdefs.Validationf(
				"graph: node %q: unknown config field %q for type %q", nd.ID, name, nd.Type)
		}
	}
	return nil
}

// Name returns the graph's name from its definition.
func (g *Graph) Name() string { return g.name }

// Entry returns the entry node id.
func (g *Graph) Entry() string { return g.entry }

// NodeIDs returns all node ids in definition order.
func (g *Graph) NodeIDs() []string {
	out := make([]string, len(g.order))
	copy(out, g.order)
	return out
}

// EdgesFrom returns the compiled outgoing edges of a node.
func (g *Graph) EdgesFrom(nodeID string) []Edge {
	out := g.edges[nodeID]
	cp := make([]Edge, len(out))
	copy(cp, out)
	return cp
}

// Warnings returns the build-time topology findings.
func (g *Graph) Warnings() []Warning {
	out := make([]Warning, len(g.warnings))
	copy(out, g.warnings)
	return out
}

// Stats summarises the built graph.
func (g *Graph) Stats() Stats {
	types := map[string]bool{}
	edgeCount := 0
	for _, id := range g.order {
		types[g.nodes[id].def.Type] = true
		edgeCount += len(g.edges[id])
	}
	return Stats{
		Nodes:           len(g.order),
		Edges:           edgeCount,
		NodeTypes:       len(types),
		MaxIterations:   g.maxIterations,
		ParallelEnabled: g.parallel.Enabled,
	}
}

// Compile-time interface assertions: a *Graph is a resumable engine.
var (
	_ agent.Engine  = (*Graph)(nil)
	_ agent.Resumer = (*Graph)(nil)
)
