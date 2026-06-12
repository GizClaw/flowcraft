package executor

import (
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
	"github.com/GizClaw/flowcraft/sdk/embedding"
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

	Index            retrieval.Index
	Embedder         embedding.Embedder
	EmbeddingTimeout time.Duration

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

	index            retrieval.Index
	embedder         embedding.Embedder
	embeddingTimeout time.Duration

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

// DocumentChunkSearchResponse hydrates document chunk search hits.
type DocumentChunkSearchResponse struct {
	Hits []DocumentChunkSearchHit
	Took time.Duration
}

// SummaryNodeSearchResponse hydrates summary node search hits.
type SummaryNodeSearchResponse struct {
	Hits []SummaryNodeSearchHit
	Took time.Duration
}

// ObservationSearchResponse hydrates observation search hits.
type ObservationSearchResponse struct {
	Hits []ObservationSearchHit
	Took time.Duration
}

// FactSearchResponse hydrates fact ledger search hits.
type FactSearchResponse struct {
	Hits []FactSearchHit
	Took time.Duration
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

// EntityTimelineSearchResponse hydrates entity timeline search hits.
type EntityTimelineSearchResponse struct {
	Hits []EntityTimelineSearchHit
	Took time.Duration
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
