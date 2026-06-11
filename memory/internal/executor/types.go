package executor

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/internal/compiler"
	"github.com/GizClaw/flowcraft/memory/internal/views/indexed"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcedocument "github.com/GizClaw/flowcraft/memory/sources/document"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	viewentity "github.com/GizClaw/flowcraft/memory/views/entity"
	"github.com/GizClaw/flowcraft/memory/views/fact"
	viewobservation "github.com/GizClaw/flowcraft/memory/views/observation"
	"github.com/GizClaw/flowcraft/memory/views/recent"
)

const errPrefix = "memory/internal/executor"

// Deps contains the canonical stores, semantic view stores, retrieval index,
// and capability services used to construct one memory executor.
type Deps struct {
	Assembly compiler.Assembly

	MessageStore  sourcemessage.Store
	DocumentStore sourcedocument.Store

	SummaryStore        recent.SummaryStore
	ChunkStore          viewdocument.ChunkStore
	ObservationStore    viewobservation.Store
	FactStore           fact.Store
	FactGraphStore      fact.GraphStore
	EntityProfileStore  viewentity.ProfileStore
	EntityTimelineStore viewentity.TimelineStore

	Index retrieval.Index

	DocumentChunker       DocumentChunker
	Summarizer            Summarizer
	ObservationExtractor  ObservationExtractor
	FactReconciler        FactReconciler
	FactGraphBuilder      FactGraphBuilder
	EntityProfileBuilder  EntityProfileBuilder
	EntityTimelineBuilder EntityTimelineBuilder
	ContextPacker         ContextPacker
}

// Executor is the single internal capability runner assembled from compiler output.
type Executor struct {
	assembly compiler.Assembly

	messageStore  sourcemessage.Store
	documentStore sourcedocument.Store

	recentWindow      *recent.Window
	summaryDAG        *recent.SummaryDAG
	documentChunks    *viewdocument.Chunks
	observationLedger *viewobservation.Ledger
	factLedger        *fact.Ledger
	factGraph         *fact.Graph
	entityProfile     *viewentity.Profile
	entityTimeline    *viewentity.Timeline

	index retrieval.Index

	enabled     map[compiler.Capability]compiler.ViewAssembly
	projections map[compiler.Capability]compiler.ProjectionAssembly
	writers     map[compiler.Capability]*indexed.Writer

	documentChunker       DocumentChunker
	summarizer            Summarizer
	observationExtractor  ObservationExtractor
	factReconciler        FactReconciler
	factGraphBuilder      FactGraphBuilder
	entityProfileBuilder  EntityProfileBuilder
	entityTimelineBuilder EntityTimelineBuilder
	contextPacker         ContextPacker
}

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
	Summarize(context.Context, SummaryInput) ([]recent.SummaryNode, error)
}

// SummaryInput is the evidence and view identity provided to a summary service.
type SummaryInput struct {
	View   views.Descriptor
	Scope  views.Scope
	Window recent.WindowResult
}

// ObservationExtractor derives observation records from a recent message window.
type ObservationExtractor interface {
	ExtractObservations(context.Context, ObservationInput) ([]viewobservation.Observation, error)
}

// ObservationInput is the evidence, target scope, and view identity provided to
// an observation extraction service.
type ObservationInput struct {
	View   views.Descriptor
	Window recent.WindowResult
	Scope  viewobservation.Scope
}

// FactReconciler derives durable facts from observation ledger outputs.
type FactReconciler interface {
	ReconcileFacts(context.Context, FactReconcileInput) ([]fact.Fact, error)
}

// FactReconcileInput is the evidence and view identity provided to a fact reconciler.
type FactReconcileInput struct {
	View         views.Descriptor
	Scope        views.Scope
	Observations []viewobservation.Observation
	Current      []fact.Fact
}

// FactGraphBuilder derives graph nodes and edges from reconciled facts.
type FactGraphBuilder interface {
	BuildFactGraph(context.Context, FactGraphInput) (FactGraphOutput, error)
}

// FactGraphInput is the evidence and view identity provided to a fact graph builder.
type FactGraphInput struct {
	View  views.Descriptor
	Facts []fact.Fact
}

// FactGraphOutput is the graph records produced by a FactGraphBuilder.
type FactGraphOutput struct {
	Nodes []fact.Node
	Edges []fact.Edge
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
	Facts []fact.Fact
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
	Facts []fact.Fact
	Graph FactGraphOutput
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
	Window             recent.WindowResult
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

// DocumentChunkSearchResponse hydrates document chunk search hits.
type DocumentChunkSearchResponse struct {
	Hits []DocumentChunkSearchHit
	Took time.Duration
}

// DocumentChunkSearchHit pairs a retrieval hit with its semantic chunk record.
type DocumentChunkSearchHit struct {
	Retrieval retrieval.Hit
	Chunk     viewdocument.Chunk
}

// SummaryNodeSearchResponse hydrates summary node search hits.
type SummaryNodeSearchResponse struct {
	Hits []SummaryNodeSearchHit
	Took time.Duration
}

// SummaryNodeSearchHit pairs a retrieval hit with its semantic summary node.
type SummaryNodeSearchHit struct {
	Retrieval retrieval.Hit
	Node      recent.SummaryNode
}

// ObservationSearchResponse hydrates observation search hits.
type ObservationSearchResponse struct {
	Hits []ObservationSearchHit
	Took time.Duration
}

// ObservationSearchHit pairs a retrieval hit with its semantic observation.
type ObservationSearchHit struct {
	Retrieval   retrieval.Hit
	Observation viewobservation.Observation
}

// FactSearchResponse hydrates fact ledger search hits.
type FactSearchResponse struct {
	Hits []FactSearchHit
	Took time.Duration
}

// FactSearchHit pairs a retrieval hit with its semantic fact record.
type FactSearchHit struct {
	Retrieval retrieval.Hit
	Fact      fact.Fact
}

// FactGraphBuildResult contains the stored graph records produced from facts.
type FactGraphBuildResult struct {
	Nodes []fact.Node
	Edges []fact.Edge
}

// FactGraphSearchResponse hydrates fact graph search hits.
type FactGraphSearchResponse struct {
	Hits []FactGraphSearchHit
	Took time.Duration
}

// FactGraphSearchHit pairs a retrieval hit with either a graph node or edge.
type FactGraphSearchHit struct {
	Retrieval retrieval.Hit
	Node      *fact.Node
	Edge      *fact.Edge
}

// EntityBuildInput carries fact and graph evidence for entity profile/timeline builders.
type EntityBuildInput struct {
	Scope views.Scope
	Facts []fact.Fact
	Graph *FactGraphBuildResult
}

// EntityProfileSearchResponse hydrates entity profile search hits.
type EntityProfileSearchResponse struct {
	Hits []EntityProfileSearchHit
	Took time.Duration
}

// EntityProfileSearchHit pairs a retrieval hit with its semantic entity profile.
type EntityProfileSearchHit struct {
	Retrieval retrieval.Hit
	Profile   viewentity.ProfileRecord
}

// EntityTimelineSearchResponse hydrates entity timeline search hits.
type EntityTimelineSearchResponse struct {
	Hits []EntityTimelineSearchHit
	Took time.Duration
}

// EntityTimelineSearchHit pairs a retrieval hit with its semantic entity timeline event.
type EntityTimelineSearchHit struct {
	Retrieval retrieval.Hit
	Event     viewentity.Event
}

// PackContextRequest describes the read-time evidence Executor should compose.
type PackContextRequest struct {
	Scope views.Scope
	Query string

	Window               recent.WindowRequest
	SummarySearch        *retrieval.SearchRequest
	DocumentSearch       *retrieval.SearchRequest
	ObservationSearch    *retrieval.SearchRequest
	FactSearch           *retrieval.SearchRequest
	FactGraphSearch      *retrieval.SearchRequest
	EntityProfileSearch  *retrieval.SearchRequest
	EntityTimelineSearch *retrieval.SearchRequest

	SummaryNamespace        string
	DocumentNamespace       string
	ObservationNamespace    string
	FactNamespace           string
	FactGraphNamespace      string
	EntityProfileNamespace  string
	EntityTimelineNamespace string
}

// ContextPack is a minimal deterministic composition of recent messages and
// explicitly requested retrieval results.
type ContextPack struct {
	Window             recent.WindowResult
	SummaryHits        []SummaryNodeSearchHit
	DocumentHits       []DocumentChunkSearchHit
	ObservationHits    []ObservationSearchHit
	FactHits           []FactSearchHit
	FactGraphHits      []FactGraphSearchHit
	EntityProfileHits  []EntityProfileSearchHit
	EntityTimelineHits []EntityTimelineSearchHit
	Items              []ContextItem
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

// ContextItem is one rendered, hydrated item in a ContextPack.
type ContextItem struct {
	Kind          ContextItemKind
	Text          string
	Message       *sourcemessage.Message
	SummaryNode   *recent.SummaryNode
	DocumentChunk *viewdocument.Chunk
	Observation   *viewobservation.Observation
	Fact          *fact.Fact
	FactGraphNode *fact.Node
	FactGraphEdge *fact.Edge
	EntityProfile *viewentity.ProfileRecord
	EntityEvent   *viewentity.Event
	Retrieval     *retrieval.Hit
}
