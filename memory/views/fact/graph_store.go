package fact

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const graphErrPrefix = "memory/views/fact/graph"

// NodeID is a stable fact graph node identifier.
type NodeID string

// EdgeID is a stable fact graph edge identifier.
type EdgeID string

// NodeKind describes the minimal fact graph node families needed by recall.
type NodeKind string

const (
	NodeEntity NodeKind = "entity"
	NodeValue  NodeKind = "value"
)

// FactRef links graph records back to the fact ledger records that support them.
type FactRef struct {
	FactID FactID
	Role   string
}

// Node is an entity or value participating in long-lived fact recall.
type Node struct {
	ID         NodeID
	Kind       NodeKind
	Label      string
	Aliases    []string
	FactRefs   []FactRef
	SourceRefs []views.SourceRef
	Signature  views.ViewSignature
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Metadata   map[string]any
}

// Edge is a predicate relation between two fact graph nodes.
type Edge struct {
	ID         EdgeID
	From       NodeID
	To         NodeID
	Predicate  string
	Status     FactStatus
	Confidence float64
	ValidFrom  *time.Time
	ValidUntil *time.Time
	FactRefs   []FactRef
	SourceRefs []views.SourceRef
	Signature  views.ViewSignature
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Metadata   map[string]any
}

// NodeListOptions controls deterministic node scans.
type NodeListOptions struct {
	AfterID NodeID
	Limit   int
	Kind    *NodeKind
	Label   string
}

// EdgeListOptions controls deterministic edge scans.
type EdgeListOptions struct {
	AfterID   EdgeID
	Limit     int
	From      NodeID
	To        NodeID
	Predicate string
	Status    *FactStatus
}

// GraphStore persists fact graph nodes and edges.
type GraphStore interface {
	PutNode(ctx context.Context, node Node) (Node, error)
	GetNode(ctx context.Context, id NodeID) (Node, bool, error)
	ListNodes(ctx context.Context, opts NodeListOptions) ([]Node, error)
	DeleteNode(ctx context.Context, id NodeID) error
	PutEdge(ctx context.Context, edge Edge) (Edge, error)
	GetEdge(ctx context.Context, id EdgeID) (Edge, bool, error)
	ListEdges(ctx context.Context, opts EdgeListOptions) ([]Edge, error)
	DeleteEdge(ctx context.Context, id EdgeID) error
}

func validateNode(node Node) error {
	if node.ID == "" {
		return errdefs.Validationf("%s: node id is required", graphErrPrefix)
	}
	if err := validateNodeKind(node.Kind); err != nil {
		return err
	}
	if node.Label == "" {
		return errdefs.Validationf("%s: node label is required", graphErrPrefix)
	}
	if len(node.FactRefs) == 0 {
		return errdefs.Validationf("%s: fact_refs are required", graphErrPrefix)
	}
	for _, ref := range node.FactRefs {
		if err := validateFactRef(ref); err != nil {
			return err
		}
	}
	for _, ref := range node.SourceRefs {
		if err := ref.Validate(); err != nil {
			return err
		}
	}
	return validateGraphSignature(node.Signature)
}

func validateEdge(edge Edge) error {
	if edge.ID == "" {
		return errdefs.Validationf("%s: edge id is required", graphErrPrefix)
	}
	if edge.From == "" {
		return errdefs.Validationf("%s: edge from node is required", graphErrPrefix)
	}
	if edge.To == "" {
		return errdefs.Validationf("%s: edge to node is required", graphErrPrefix)
	}
	if edge.Predicate == "" {
		return errdefs.Validationf("%s: predicate is required", graphErrPrefix)
	}
	if err := edge.Status.Validate(); err != nil {
		return err
	}
	if edge.Confidence != 0 && (edge.Confidence < 0 || edge.Confidence > 1) {
		return errdefs.Validationf("%s: confidence must be between 0 and 1", graphErrPrefix)
	}
	if edge.ValidFrom != nil && edge.ValidUntil != nil && edge.ValidUntil.Before(*edge.ValidFrom) {
		return errdefs.Validationf("%s: valid_until must be greater than or equal to valid_from", graphErrPrefix)
	}
	if len(edge.FactRefs) == 0 {
		return errdefs.Validationf("%s: fact_refs are required", graphErrPrefix)
	}
	for _, ref := range edge.FactRefs {
		if err := validateFactRef(ref); err != nil {
			return err
		}
	}
	for _, ref := range edge.SourceRefs {
		if err := ref.Validate(); err != nil {
			return err
		}
	}
	return validateGraphSignature(edge.Signature)
}

func validateGraphSignature(signature views.ViewSignature) error {
	if signature.IsZero() {
		return errdefs.Validationf("%s: signature is required", graphErrPrefix)
	}
	if len(signature.UpstreamViewRefs) == 0 {
		return errdefs.Validationf("%s: upstream fact ledger view refs are required", graphErrPrefix)
	}
	if err := signature.Validate(); err != nil {
		return err
	}
	return nil
}

func validateFactRef(ref FactRef) error {
	if ref.FactID == "" {
		return errdefs.Validationf("%s: fact ref fact id is required", graphErrPrefix)
	}
	return nil
}

func validateNodeID(id NodeID) error {
	if id == "" {
		return errdefs.Validationf("%s: node id is required", graphErrPrefix)
	}
	return nil
}

func validateEdgeID(id EdgeID) error {
	if id == "" {
		return errdefs.Validationf("%s: edge id is required", graphErrPrefix)
	}
	return nil
}

func validateNodeKind(kind NodeKind) error {
	switch kind {
	case NodeEntity, NodeValue:
		return nil
	default:
		return errdefs.Validationf("%s: unsupported node kind %q", graphErrPrefix, kind)
	}
}

func validateNodeListOptions(opts NodeListOptions) error {
	if opts.Kind != nil {
		if err := validateNodeKind(*opts.Kind); err != nil {
			return err
		}
	}
	return nil
}

func validateEdgeListOptions(opts EdgeListOptions) error {
	if opts.Status != nil {
		if err := opts.Status.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func normalizeNode(node Node) Node {
	if node.Kind == "" {
		node.Kind = NodeEntity
	}
	return node
}

func normalizeEdge(edge Edge) Edge {
	if edge.Status == "" {
		edge.Status = FactActive
	}
	return edge
}

func normalizeNodeListOptions(opts NodeListOptions) NodeListOptions {
	if opts.Kind != nil && *opts.Kind == "" {
		kind := NodeEntity
		opts.Kind = &kind
	}
	return opts
}

func normalizeEdgeListOptions(opts EdgeListOptions) EdgeListOptions {
	if opts.Status != nil && *opts.Status == "" {
		status := FactActive
		opts.Status = &status
	}
	return opts
}

func cloneNodeListOptions(in NodeListOptions) NodeListOptions {
	out := in
	if in.Kind != nil {
		kind := *in.Kind
		out.Kind = &kind
	}
	return out
}

func cloneEdgeListOptions(in EdgeListOptions) EdgeListOptions {
	out := in
	if in.Status != nil {
		status := *in.Status
		out.Status = &status
	}
	return out
}
