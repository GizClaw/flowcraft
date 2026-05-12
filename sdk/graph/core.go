// Package graph implements the core graph engine for FlowCraft.
//
// A Graph is a compiled, immutable directed graph of Nodes connected by Edges.
// Nodes operate on a shared Board and pass control to successor node(s). Edges
// may carry compiled conditions (evaluated by expr-lang/expr) for dynamic
// routing.
//
// File layout in this package:
//
//	core.go        engine sentinels, Node interface family, ExecutionContext, Graph
//	definition.go  declarative GraphDefinition / RawGraph / GraphMeta
//	port.go        typed input/output ports + runtime validation
//	board.go       blackboard alias to engine.Board
//	condition.go   compiled boolean expressions for edge / skip conditions
//	stream.go      StreamPublisher abstraction handed to nodes
//	vars.go        well-known board variable keys
package graph

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/engine"
)

// ---------------------------------------------------------------------------
// Engine sentinels
// ---------------------------------------------------------------------------

// END is a sentinel node ID that marks the end of execution.
const END = "__end__"

// ---------------------------------------------------------------------------
// Node interface family
//
// Node is the only required interface. The optional interfaces below let
// nodes opt into deferred config resolution, self-description, or typed port
// declarations; the executor probes for them via type assertions.
// ---------------------------------------------------------------------------

// Node is the interface that all graph nodes must implement.
type Node interface {
	ID() string
	Type() string
	ExecuteBoard(ctx ExecutionContext, board *Board) error
}

// Configurable is an optional interface for nodes whose config can be
// dynamically resolved (e.g. variable reference expansion).
type Configurable interface {
	SetConfig(config map[string]any)
	Config() map[string]any
}

// Describable is an optional interface for nodes that provide a description.
type Describable interface {
	Description() string
}

// PortDeclarable is an optional interface for nodes that declare typed
// input/output ports for compile-time and runtime validation.
type PortDeclarable interface {
	InputPorts() []Port
	OutputPorts() []Port
}

// ---------------------------------------------------------------------------
// Execution context
// ---------------------------------------------------------------------------

// ExecutionContext wraps a standard context.Context with graph execution metadata.
//
// Host is the engine.Host the executor was started with; nodes use it for
// cooperative interrupts (Host.Interrupts()), user prompts (Host.AskUser),
// usage reporting and checkpointing. The executor guarantees Host is non-nil
// (it falls back to engine.NoopHost{}) so nodes never need a nil check.
//
// Publisher is a thin wrapper around Host.Publish kept for backwards
// compatibility and ergonomic event emission with (type, payload) pairs;
// new code MAY call Host.Publish directly with a fully formed envelope.
//
// Deps is the typed dependency container the upstream agent.Run handed
// to the engine via [engine.Run.Deps]. The graph runner propagates it
// verbatim so nodes can recover host-supplied dependencies (LLM clients,
// tool registries, retrievers, …) via [engine.GetDep] instead of
// closure-binding them at builder time. May be nil when the engine was
// invoked directly without [engine.Run.Deps]; nodes that need a dep MUST
// handle the nil case (engine.GetDep does).
//
// Attributes is the read-only string-keyed bag that the upstream
// agent.Run promoted from [agent.Request] / [agent.RunInfo]. Canonical
// keys live under [github.com/GizClaw/flowcraft/sdk/telemetry] (e.g.
// AttrAgentID / AttrTaskID / AttrContextID) so nodes that need
// agent-scoped identity (scriptnode RunInfoBridge, telemetry hooks,
// envelope agent_id) can read them without inventing a parallel
// transport. May be nil when the engine was invoked without attributes.
type ExecutionContext struct {
	Context    context.Context
	Host       engine.Host
	Publisher  StreamPublisher
	RunID      string
	Deps       *engine.Dependencies
	Attributes map[string]string
}

// ---------------------------------------------------------------------------
// Graph + Edge (immutable runtime form)
// ---------------------------------------------------------------------------

// Graph is an immutable, executable directed graph of nodes.
//
// It can only be created via NewGraph (called by Assemble) and provides
// read-only accessors. The executor is the sole consumer.
type Graph struct {
	name           string
	entry          string
	nodes          map[string]Node
	edges          map[string][]Edge
	reverse        map[string][]string
	skipConditions map[string]*CompiledCondition
	meta           GraphMeta
}

// Edge represents a directed connection between two nodes. A nil Condition
// means the edge is unconditional; otherwise the executor evaluates it
// against the current Board to decide whether to follow the edge.
type Edge struct {
	From      string
	To        string
	Condition *CompiledCondition
}

// NewGraph constructs an immutable Graph. Intended to be called by the
// Assemble step (graph/runner), not by end users directly.
func NewGraph(raw *RawGraph, meta GraphMeta) *Graph {
	return &Graph{
		name:           raw.Name,
		entry:          raw.Entry,
		nodes:          raw.Nodes,
		edges:          raw.Edges,
		reverse:        raw.Reverse,
		skipConditions: raw.SkipConditions,
		meta:           meta,
	}
}

// --- Graph accessors ---

func (g *Graph) Name() string  { return g.name }
func (g *Graph) Entry() string { return g.entry }

func (g *Graph) Node(id string) (Node, bool) {
	n, ok := g.nodes[id]
	return n, ok
}

func (g *Graph) Edges(from string) []Edge { return g.edges[from] }

func (g *Graph) AllEdges() []Edge {
	var all []Edge
	for _, edges := range g.edges {
		all = append(all, edges...)
	}
	return all
}

func (g *Graph) Reverse(to string) []string { return g.reverse[to] }

func (g *Graph) SkipCondition(id string) (*CompiledCondition, bool) {
	c, ok := g.skipConditions[id]
	return c, ok
}

func (g *Graph) Meta() GraphMeta { return g.meta }

// ---------------------------------------------------------------------------
// Built-in nodes
// ---------------------------------------------------------------------------

// passthroughNode is a no-op node used for __end__ and similar sentinels.
type passthroughNode struct {
	id  string
	typ string
}

func (n *passthroughNode) ID() string   { return n.id }
func (n *passthroughNode) Type() string { return n.typ }
func (n *passthroughNode) ExecuteBoard(_ ExecutionContext, _ *Board) error {
	return nil
}

// NewPassthroughNode creates a no-op node with the given ID and type.
func NewPassthroughNode(id, typ string) Node {
	return &passthroughNode{id: id, typ: typ}
}
