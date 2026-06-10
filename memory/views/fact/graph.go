package fact

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const (
	// DefaultGraphID is the descriptor ID used by NewGraph unless overridden.
	DefaultGraphID views.ID = "fact-graph"

	// DefaultGraphVersion is the descriptor version used by NewGraph unless overridden.
	DefaultGraphVersion = "v1"
)

// GraphOption configures a Graph.
type GraphOption interface {
	applyGraph(*Graph)
}

type graphDescriptorOption struct {
	id      views.ID
	version string
}

// WithGraphID overrides the descriptor ID for Graph.
func WithGraphID(id views.ID) GraphOption {
	return graphDescriptorOption{id: id}
}

// WithGraphVersion overrides the descriptor version for Graph.
func WithGraphVersion(version string) GraphOption {
	return graphDescriptorOption{version: version}
}

func (o graphDescriptorOption) applyGraph(g *Graph) {
	if o.id != "" {
		g.id = o.id
	}
	if o.version != "" {
		g.version = o.version
	}
}

// Graph is a lightweight facade for the fact graph view contract.
//
// It persists entity/value nodes and predicate edges derived from fact ledger
// outputs. The graph is a semantic view, not a retrieval/index projection.
type Graph struct {
	store   GraphStore
	id      views.ID
	version string
}

var _ views.View = (*Graph)(nil)

// NewGraph creates a fact graph view backed by store.
func NewGraph(store GraphStore, opts ...GraphOption) *Graph {
	graph := &Graph{
		store:   store,
		id:      DefaultGraphID,
		version: DefaultGraphVersion,
	}
	for _, opt := range opts {
		if opt != nil {
			opt.applyGraph(graph)
		}
	}
	return graph
}

// Descriptor declares the Graph view identity.
func (g *Graph) Descriptor() views.Descriptor {
	return views.Descriptor{
		ID:      g.id,
		Kind:    views.KindFactGraph,
		Version: g.version,
	}
}

// PutNode stores or replaces a graph node. Empty kind is normalized to entity.
func (g *Graph) PutNode(ctx context.Context, node Node) (Node, error) {
	if g.store == nil {
		return Node{}, errdefs.Validationf("%s: store is required", graphErrPrefix)
	}
	node = normalizeNode(cloneNode(node))
	if err := validateNode(node); err != nil {
		return Node{}, err
	}
	stored, err := g.store.PutNode(ctx, node)
	if err != nil {
		return Node{}, err
	}
	return cloneNode(stored), nil
}

// GetNode returns one graph node by id.
func (g *Graph) GetNode(ctx context.Context, id NodeID) (Node, bool, error) {
	if g.store == nil {
		return Node{}, false, errdefs.Validationf("%s: store is required", graphErrPrefix)
	}
	if err := validateNodeID(id); err != nil {
		return Node{}, false, err
	}
	node, ok, err := g.store.GetNode(ctx, id)
	if err != nil {
		return Node{}, false, err
	}
	if !ok {
		return Node{}, false, nil
	}
	return cloneNode(node), true, nil
}

// ListNodes returns graph nodes matching opts.
func (g *Graph) ListNodes(ctx context.Context, opts NodeListOptions) ([]Node, error) {
	if g.store == nil {
		return nil, errdefs.Validationf("%s: store is required", graphErrPrefix)
	}
	opts = normalizeNodeListOptions(cloneNodeListOptions(opts))
	if err := validateNodeListOptions(opts); err != nil {
		return nil, err
	}
	nodes, err := g.store.ListNodes(ctx, opts)
	if err != nil {
		return nil, err
	}
	return cloneNodes(nodes), nil
}

// DeleteNode removes a node and any edges that reference it. It is idempotent at the Store boundary.
func (g *Graph) DeleteNode(ctx context.Context, id NodeID) error {
	if g.store == nil {
		return errdefs.Validationf("%s: store is required", graphErrPrefix)
	}
	if err := validateNodeID(id); err != nil {
		return err
	}
	return g.store.DeleteNode(ctx, id)
}

// PutEdge stores or replaces a graph edge. Empty status is normalized to active.
func (g *Graph) PutEdge(ctx context.Context, edge Edge) (Edge, error) {
	if g.store == nil {
		return Edge{}, errdefs.Validationf("%s: store is required", graphErrPrefix)
	}
	edge = normalizeEdge(cloneEdge(edge))
	if err := validateEdge(edge); err != nil {
		return Edge{}, err
	}
	stored, err := g.store.PutEdge(ctx, edge)
	if err != nil {
		return Edge{}, err
	}
	return cloneEdge(stored), nil
}

// GetEdge returns one graph edge by id.
func (g *Graph) GetEdge(ctx context.Context, id EdgeID) (Edge, bool, error) {
	if g.store == nil {
		return Edge{}, false, errdefs.Validationf("%s: store is required", graphErrPrefix)
	}
	if err := validateEdgeID(id); err != nil {
		return Edge{}, false, err
	}
	edge, ok, err := g.store.GetEdge(ctx, id)
	if err != nil {
		return Edge{}, false, err
	}
	if !ok {
		return Edge{}, false, nil
	}
	return cloneEdge(edge), true, nil
}

// ListEdges returns graph edges matching opts.
func (g *Graph) ListEdges(ctx context.Context, opts EdgeListOptions) ([]Edge, error) {
	if g.store == nil {
		return nil, errdefs.Validationf("%s: store is required", graphErrPrefix)
	}
	opts = normalizeEdgeListOptions(cloneEdgeListOptions(opts))
	if err := validateEdgeListOptions(opts); err != nil {
		return nil, err
	}
	edges, err := g.store.ListEdges(ctx, opts)
	if err != nil {
		return nil, err
	}
	return cloneEdges(edges), nil
}

// DeleteEdge removes one edge by id. It is idempotent at the Store boundary.
func (g *Graph) DeleteEdge(ctx context.Context, id EdgeID) error {
	if g.store == nil {
		return errdefs.Validationf("%s: store is required", graphErrPrefix)
	}
	if err := validateEdgeID(id); err != nil {
		return err
	}
	return g.store.DeleteEdge(ctx, id)
}

func cloneNode(in Node) Node {
	out := in
	out.Aliases = cloneStrings(in.Aliases)
	out.FactRefs = cloneFactRefs(in.FactRefs)
	out.SourceRefs = cloneSourceRefs(in.SourceRefs)
	out.Signature = cloneViewSignature(in.Signature)
	if in.Metadata != nil {
		out.Metadata = cloneAnyMap(in.Metadata)
	}
	return out
}

func cloneNodes(in []Node) []Node {
	if in == nil {
		return nil
	}
	out := make([]Node, len(in))
	for i, node := range in {
		out[i] = cloneNode(node)
	}
	return out
}

func cloneEdge(in Edge) Edge {
	out := in
	out.ValidFrom = cloneTimePtr(in.ValidFrom)
	out.ValidUntil = cloneTimePtr(in.ValidUntil)
	out.FactRefs = cloneFactRefs(in.FactRefs)
	out.SourceRefs = cloneSourceRefs(in.SourceRefs)
	out.Signature = cloneViewSignature(in.Signature)
	if in.Metadata != nil {
		out.Metadata = cloneAnyMap(in.Metadata)
	}
	return out
}

func cloneEdges(in []Edge) []Edge {
	if in == nil {
		return nil
	}
	out := make([]Edge, len(in))
	for i, edge := range in {
		out[i] = cloneEdge(edge)
	}
	return out
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string(nil), in...)
}

func cloneFactRefs(in []FactRef) []FactRef {
	if in == nil {
		return nil
	}
	return append([]FactRef(nil), in...)
}
