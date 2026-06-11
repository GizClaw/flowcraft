package recent

import (
	"maps"
	"time"

	"github.com/GizClaw/flowcraft/memory/views"
)

// NodeID is a stable summary-node identifier within a conversation.
type NodeID string

// SummaryNode is one node in a summary DAG derived from MessageLog evidence.
//
// Metadata must be JSON-compatible. Values roundtrip through encoding/json, so
// decoded maps use map[string]any, arrays use []any, and numbers use float64.
type SummaryNode struct {
	ID         NodeID
	Scope      views.Scope
	ParentIDs  []NodeID
	SourceRefs []views.SourceRef
	Summary    string
	Level      int
	Signature  views.ViewSignature
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Metadata   map[string]any
}

func cloneSummaryNode(node SummaryNode) SummaryNode {
	if node.ParentIDs != nil {
		node.ParentIDs = append([]NodeID(nil), node.ParentIDs...)
	}
	if node.SourceRefs != nil {
		sourceRefs := node.SourceRefs
		node.SourceRefs = make([]views.SourceRef, len(sourceRefs))
		for i, ref := range sourceRefs {
			node.SourceRefs[i] = cloneSourceRef(ref)
		}
	}
	if node.Signature.SourceRevisions != nil {
		node.Signature.SourceRevisions = append([]views.SourceRevision(nil), node.Signature.SourceRevisions...)
	}
	if node.Signature.UpstreamViewRefs != nil {
		node.Signature.UpstreamViewRefs = append([]views.UpstreamViewRef(nil), node.Signature.UpstreamViewRefs...)
	}
	if node.Signature.DiagnosticSignatures != nil {
		node.Signature.DiagnosticSignatures = maps.Clone(node.Signature.DiagnosticSignatures)
	}
	if node.Metadata != nil {
		node.Metadata = maps.Clone(node.Metadata)
	}
	return node
}

func cloneSourceRef(ref views.SourceRef) views.SourceRef {
	if ref.Message != nil {
		msg := *ref.Message
		if msg.Span != nil {
			span := *msg.Span
			msg.Span = &span
		}
		ref.Message = &msg
	}
	if ref.Document != nil {
		doc := *ref.Document
		if doc.Span != nil {
			span := *doc.Span
			doc.Span = &span
		}
		ref.Document = &doc
	}
	return ref
}
