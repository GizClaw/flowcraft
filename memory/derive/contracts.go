// Package derive defines shared memory domain derivation and hook contracts.
package derive

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcedocument "github.com/GizClaw/flowcraft/memory/sources/document"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	viewentity "github.com/GizClaw/flowcraft/memory/views/entity"
	viewfact "github.com/GizClaw/flowcraft/memory/views/fact"
	viewobservation "github.com/GizClaw/flowcraft/memory/views/observation"
	viewrecent "github.com/GizClaw/flowcraft/memory/views/recent"
)

// DocumentChunker derives semantic chunk records from a canonical document.
type DocumentChunker interface {
	ChunkDocument(context.Context, DocumentChunkInput) ([]viewdocument.Chunk, error)
}

// DocumentChunkInput is the evidence and view identity provided to a chunking service.
type DocumentChunkInput struct {
	View     views.Descriptor
	Scope    views.Scope
	Document sourcedocument.Document
}

// Summarizer derives SummaryDAG nodes from a recent message window.
type Summarizer interface {
	Summarize(context.Context, SummaryInput) ([]viewrecent.SummaryNode, error)
}

// SummaryInput is the evidence and view identity provided to a summary service.
type SummaryInput struct {
	View   views.Descriptor
	Scope  views.Scope
	Window viewrecent.WindowResult
}

// ObservationExtractor derives observation records from a recent message window.
type ObservationExtractor interface {
	ExtractObservations(context.Context, ObservationInput) ([]viewobservation.Observation, error)
}

// ObservationInput is the evidence, target scope, and view identity provided to
// an observation extraction service.
type ObservationInput struct {
	View   views.Descriptor
	Window viewrecent.WindowResult
	Scope  viewobservation.Scope
}

// FactReconciler derives durable facts from observation ledger outputs.
type FactReconciler interface {
	ReconcileFacts(context.Context, FactReconcileInput) ([]viewfact.Fact, error)
}

// FactReconcileInput is the evidence and view identity provided to a fact reconciler.
type FactReconcileInput struct {
	View         views.Descriptor
	Scope        views.Scope
	Observations []viewobservation.Observation
	Current      []viewfact.Fact
}

// FactGraphBuilder derives graph nodes and edges from reconciled facts.
type FactGraphBuilder interface {
	BuildFactGraph(context.Context, FactGraphInput) (FactGraphOutput, error)
}

// FactGraphInput is the evidence and view identity provided to a fact graph builder.
type FactGraphInput struct {
	View  views.Descriptor
	Facts []viewfact.Fact
}

// FactGraphOutput is the graph records produced by a FactGraphBuilder.
type FactGraphOutput struct {
	Nodes []viewfact.Node
	Edges []viewfact.Edge
}

// EntityProfileBuilder derives entity profile records from fact graph and fact
// ledger outputs. It receives evidence from the executor; builders must not read stores.
type EntityProfileBuilder interface {
	BuildEntityProfiles(context.Context, EntityProfileInput) ([]viewentity.ProfileRecord, error)
}

// EntityProfileInput is the evidence and view identity provided to an entity profile builder.
type EntityProfileInput struct {
	View  views.Descriptor
	Scope views.Scope
	Facts []viewfact.Fact
	Graph FactGraphOutput
}

// EntityTimelineBuilder derives entity timeline events from fact graph and fact
// ledger outputs. It receives evidence from the executor; builders must not read stores.
type EntityTimelineBuilder interface {
	BuildEntityTimeline(context.Context, EntityTimelineInput) ([]viewentity.Event, error)
}

// EntityTimelineInput is the evidence and view identity provided to an entity timeline builder.
type EntityTimelineInput struct {
	View  views.Descriptor
	Scope views.Scope
	Facts []viewfact.Fact
	Graph FactGraphOutput
}

// DocumentChunkSearchHit pairs a retrieval hit with its semantic chunk record.
type DocumentChunkSearchHit struct {
	Retrieval retrieval.Hit
	Chunk     viewdocument.Chunk
}

// SummaryNodeSearchHit pairs a retrieval hit with its semantic summary node.
type SummaryNodeSearchHit struct {
	Retrieval retrieval.Hit
	Node      viewrecent.SummaryNode
}

// ObservationSearchHit pairs a retrieval hit with its semantic observation.
type ObservationSearchHit struct {
	Retrieval   retrieval.Hit
	Observation viewobservation.Observation
}

// FactSearchHit pairs a retrieval hit with its semantic fact record.
type FactSearchHit struct {
	Retrieval retrieval.Hit
	Fact      viewfact.Fact
}

// FactGraphSearchHit pairs a retrieval hit with either a graph node or edge.
type FactGraphSearchHit struct {
	Retrieval  retrieval.Hit
	Node       *viewfact.Node
	Edge       *viewfact.Edge
	Expanded   bool
	Depth      int
	SeedNodeID viewfact.NodeID
	SeedEdgeID viewfact.EdgeID
}

// EntityProfileSearchHit pairs a retrieval hit with its semantic entity profile.
type EntityProfileSearchHit struct {
	Retrieval retrieval.Hit
	Profile   viewentity.ProfileRecord
}

// EntityTimelineSearchHit pairs a retrieval hit with its semantic entity timeline event.
type EntityTimelineSearchHit struct {
	Retrieval retrieval.Hit
	Event     viewentity.Event
}

// ContextItemKind identifies the source of a packed context item.
type ContextItemKind string

const (
	ContextItemRecentMessage  ContextItemKind = "recent_message"
	ContextItemSummaryNode    ContextItemKind = "summary_node"
	ContextItemDocumentChunk  ContextItemKind = "document_chunk"
	ContextItemObservation    ContextItemKind = "observation"
	ContextItemFact           ContextItemKind = "fact"
	ContextItemFactGraphNode  ContextItemKind = "fact_graph_node"
	ContextItemFactGraphEdge  ContextItemKind = "fact_graph_edge"
	ContextItemEntityProfile  ContextItemKind = "entity_profile"
	ContextItemEntityTimeline ContextItemKind = "entity_timeline"
)

// ContextItem is one rendered, hydrated item in a context pack.
type ContextItem struct {
	Kind          ContextItemKind
	Text          string
	Message       *sourcemessage.Message
	SummaryNode   *viewrecent.SummaryNode
	DocumentChunk *viewdocument.Chunk
	Observation   *viewobservation.Observation
	Fact          *viewfact.Fact
	FactGraphNode *viewfact.Node
	FactGraphEdge *viewfact.Edge
	EntityProfile *viewentity.ProfileRecord
	EntityEvent   *viewentity.Event
	Retrieval     *retrieval.Hit
}

// ContextPacker optionally chooses the final context items from executor-built
// candidates. It receives only typed DTO evidence and must not read stores.
type ContextPacker interface {
	PackContext(context.Context, ContextPackInput) (ContextPackOutput, error)
}

// ContextPackInput carries deterministic candidate evidence for a packer hook.
type ContextPackInput struct {
	Scope              views.Scope
	Query              string
	Window             viewrecent.WindowResult
	Items              []ContextItem
	SummaryHits        []SummaryNodeSearchHit
	DocumentHits       []DocumentChunkSearchHit
	ObservationHits    []ObservationSearchHit
	FactHits           []FactSearchHit
	FactGraphHits      []FactGraphSearchHit
	EntityProfileHits  []EntityProfileSearchHit
	EntityTimelineHits []EntityTimelineSearchHit
}

// ContextPackOutput contains the final items selected by a packer hook.
// An empty or nil Items slice is a valid result and filters all context items.
type ContextPackOutput struct {
	Items []ContextItem
}
