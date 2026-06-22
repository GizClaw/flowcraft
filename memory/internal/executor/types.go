package executor

import (
	"time"

	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/internal/compiler"
	"github.com/GizClaw/flowcraft/memory/internal/views/indexed"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcedocument "github.com/GizClaw/flowcraft/memory/sources/document"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	viewentityfact "github.com/GizClaw/flowcraft/memory/views/entityfact"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/embedding"
)

const errPrefix = "memory/internal/executor"

// Deps contains the canonical stores, retained view stores, retrieval index,
// and capability services used to construct one memory executor.
type Deps struct {
	Assembly compiler.Assembly

	MessageStore  sourcemessage.Store
	DocumentStore sourcedocument.Store

	SummaryStore    recent.SummaryStore
	ChunkStore      viewdocument.ChunkStore
	EntityFactStore viewentityfact.Store

	Index            retrieval.Index
	Embedder         embedding.Embedder
	EmbeddingTimeout time.Duration

	DocumentChunker     derive.DocumentChunker
	Summarizer          derive.Summarizer
	EntityFactExtractor derive.EntityFactExtractor
	ContextPacker       derive.ContextPacker
}

// Executor is the single internal capability runner assembled from compiler output.
type Executor struct {
	assembly compiler.Assembly

	messageStore  sourcemessage.Store
	documentStore sourcedocument.Store

	recentWindow   *recent.Window
	summaryDAG     *recent.SummaryDAG
	documentChunks *viewdocument.Chunks
	entityFacts    *viewentityfact.Graph

	index            retrieval.Index
	embedder         embedding.Embedder
	embeddingTimeout time.Duration

	enabled     map[compiler.Capability]compiler.ViewAssembly
	projections map[compiler.Capability]compiler.ProjectionAssembly
	writers     map[compiler.Capability]*indexed.Writer

	documentChunker     derive.DocumentChunker
	summarizer          derive.Summarizer
	entityFactExtractor derive.EntityFactExtractor
	contextPacker       derive.ContextPacker
}

// DocumentChunkSearchResponse hydrates document chunk search hits.
type DocumentChunkSearchResponse struct {
	Hits []derive.DocumentChunkSearchHit
	Took time.Duration
}

// SummaryNodeSearchResponse hydrates summary node search hits.
type SummaryNodeSearchResponse struct {
	Hits []derive.SummaryNodeSearchHit
	Took time.Duration
}

// EntityFactSearchResponse hydrates entity fact search hits.
type EntityFactSearchResponse struct {
	Hits []derive.EntityFactSearchHit
	Took time.Duration
}

// SourceMessageSearchResponse hydrates source message search hits.
type SourceMessageSearchResponse struct {
	Hits []derive.SourceMessageSearchHit
	Took time.Duration
}

// PackContextRequest describes the read-time evidence Executor should compose.
type PackContextRequest struct {
	Scope views.Scope
	Query string

	Window           recent.WindowRequest
	MessageSearch    *retrieval.SearchRequest
	SummarySearch    *retrieval.SearchRequest
	DocumentSearch   *retrieval.SearchRequest
	EntityFactSearch *retrieval.SearchRequest

	SummaryRetrieval SummaryRetrievalConfig

	MessageNamespace    string
	SummaryNamespace    string
	DocumentNamespace   string
	EntityFactNamespace string
}

// SummaryRetrievalConfig controls read-time SummaryDAG retrieval behavior.
type SummaryRetrievalConfig struct {
	// DrillDownMaxDepth bounds ParentIDs traversal after an initial summary hit.
	// A negative value means traverse until a terminal/leaf node; zero disables
	// drill-down; positive values allow that many child-selection steps.
	DrillDownMaxDepth int
	// DrillDownChildTopK is the number of child summaries retained per layer.
	DrillDownChildTopK int
}

// ContextPack is a minimal deterministic composition of recent messages and
// explicitly requested retrieval results.
type ContextPack struct {
	Window       recent.WindowResult
	MessageHits  []derive.SourceMessageSearchHit
	SummaryHits  []derive.SummaryNodeSearchHit
	DocumentHits []derive.DocumentChunkSearchHit
	EntityHits   []derive.EntityFactSearchHit
	Items        []derive.ContextItem
}
