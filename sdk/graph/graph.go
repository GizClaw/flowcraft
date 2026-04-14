// Package graph implements the core graph engine for FlowCraft.
//
// A Graph is a compiled, immutable directed graph of Nodes connected by Edges.
// Nodes operate on a shared Board and pass control to successor node(s).
// Edges may carry compiled conditions (evaluated by expr-lang/expr) for
// dynamic routing.
package graph

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// END is a sentinel node ID that marks the end of execution.
const END = "__end__"

// ErrInterrupt is returned by a node's ExecuteBoard to signal a graceful
// early exit. The executor stops and returns the current Board alongside
// ErrInterrupt so the caller can persist and resume later.
var ErrInterrupt = errdefs.Interrupted(errdefs.New("execution interrupted"))

// Node is the interface that all graph nodes must implement.
type Node interface {
	ID() string
	Type() string
	ExecuteBoard(ctx ExecutionContext, board *Board) error
}

// ExecutionContext wraps a standard context.Context with graph execution metadata.
type ExecutionContext struct {
	Context context.Context
	Stream  StreamCallback
	RunID   string
}

// StreamCallback is the signature for receiving streaming events during execution.
type StreamCallback = workflow.StreamCallback

// StreamEvent carries a streaming event emitted by a node during execution.
type StreamEvent = workflow.StreamEvent

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

// Graph is an immutable, executable directed graph of nodes.
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

// NewGraph constructs an immutable Graph. This is intended to be called
// by the Assemble step, not by end users directly.
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

// Edge represents a directed connection between two nodes.
type Edge struct {
	From      string
	To        string
	Condition *CompiledCondition
}

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
