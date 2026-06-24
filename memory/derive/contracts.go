// Package derive defines shared memory derivation and hook contracts.
package derive

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcedocument "github.com/GizClaw/flowcraft/memory/sources/document"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	viewentityfact "github.com/GizClaw/flowcraft/memory/views/entityfact"
	viewrecent "github.com/GizClaw/flowcraft/memory/views/recent"
)

// DocumentChunker derives chunk records from a canonical document.
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
	View    views.Descriptor
	Scope   views.Scope
	Window  viewrecent.WindowResult
	Current []viewrecent.SummaryNode
	Policy  SummaryPolicy
}

// EntityFactExtractor derives canonical entities and source-backed facts from
// message windows.
type EntityFactExtractor interface {
	ExtractEntityFacts(context.Context, EntityFactInput) (EntityFactOutput, error)
}

// EntityFactInput is the evidence and current graph state provided to an
// entity/fact extraction service.
type EntityFactInput struct {
	View            views.Descriptor
	Scope           views.Scope
	Window          viewrecent.WindowResult
	CurrentEntities []viewentityfact.Entity
	CurrentFacts    []viewentityfact.Fact
}

// EntityFactOutput contains newly derived or updated entity-linked facts.
type EntityFactOutput struct {
	Entities []viewentityfact.Entity
	Facts    []viewentityfact.Fact
}

// SourceMessageResolver provides read-only access to canonical source messages
// referenced by derived memories during context packing.
type SourceMessageResolver interface {
	GetSourceMessage(ctx context.Context, conversationID, messageID string) (sourcemessage.Message, bool, error)
}

// SourceMessageNeighborResolver optionally exposes canonical source messages
// adjacent to another source message in the same conversation order.
type SourceMessageNeighborResolver interface {
	SourceMessageResolver
	GetSourceMessageNeighbors(ctx context.Context, conversationID, messageID string, before, after int) ([]sourcemessage.Message, error)
}

// EntityGraphSourceResolver optionally exposes read-only entity graph expansion
// for source-evidence context packing. Implementations must use existing
// retained graph state and must not invoke planners or LLMs on the read path.
type EntityGraphSourceResolver interface {
	ExpandGraphSources(ctx context.Context, scope views.Scope, seedFacts []viewentityfact.GraphSeedFact, opts viewentityfact.GraphExpansionOptions) (viewentityfact.GraphExpansionResult, error)
}

// ContextPackOptions carries optional request-scoped packer controls.
type ContextPackOptions struct {
	SourceEvidence SourceEvidencePackOptions
}

// SourceEvidencePackOptions configures request-scoped source-message evidence
// budgets. Zero values leave the packer's configured defaults unchanged.
type SourceEvidencePackOptions struct {
	MaxDirectMessages       int
	MaxSummaryMessages      int
	MaxEntityFactMessages   int
	MaxGraphMessages        int
	MaxNeighborhoodMessages int
}

// SummaryPolicy configures summary-buffer style summarization.
type SummaryPolicy struct {
	// MaxRawMessages is the maximum recent raw message buffer size. A zero value
	// lets the summarizer choose its default.
	MaxRawMessages int
	// PreserveRecentMessages is the number of newest messages left as raw
	// window context instead of folded into a new summary. A zero value lets the
	// summarizer choose its default.
	PreserveRecentMessages int
	// MaxSummaryBytes caps deterministic summary text length. A zero value lets
	// the summarizer choose its default.
	MaxSummaryBytes int
}

// DocumentChunkSearchHit pairs a retrieval hit with its chunk record.
type DocumentChunkSearchHit struct {
	Retrieval retrieval.Hit
	Chunk     viewdocument.Chunk
}

// SummaryNodeSearchHit pairs a retrieval hit with its summary node.
type SummaryNodeSearchHit struct {
	Retrieval retrieval.Hit
	Node      viewrecent.SummaryNode
}

// EntityFactSearchHit pairs a retrieval hit with its fact record.
type EntityFactSearchHit struct {
	Retrieval retrieval.Hit
	Fact      viewentityfact.Fact
}

// SourceMessageSearchHit pairs a retrieval hit with its canonical source message.
type SourceMessageSearchHit struct {
	Retrieval retrieval.Hit
	Message   sourcemessage.Message
}

// ContextItemKind identifies the source of a packed context item.
type ContextItemKind string

const (
	ContextItemRecentMessage ContextItemKind = "recent_message"
	ContextItemSummaryNode   ContextItemKind = "summary_node"
	ContextItemDocumentChunk ContextItemKind = "document_chunk"
	ContextItemEntityFact    ContextItemKind = "entity_fact"
)

// ContextItem is one rendered, hydrated item in a context pack.
type ContextItem struct {
	Kind          ContextItemKind
	Text          string
	Message       *sourcemessage.Message
	SummaryNode   *viewrecent.SummaryNode
	DocumentChunk *viewdocument.Chunk
	EntityFact    *viewentityfact.Fact
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
	Options            ContextPackOptions
	Window             viewrecent.WindowResult
	SourceMessages     SourceMessageResolver
	EntityGraphSources EntityGraphSourceResolver
	Items              []ContextItem
	MessageHits        []SourceMessageSearchHit
	SummaryHits        []SummaryNodeSearchHit
	DocumentHits       []DocumentChunkSearchHit
	EntityHits         []EntityFactSearchHit
}

// ContextPackOutput contains the final items selected by a packer hook.
// An empty or nil Items slice is a valid result and filters all context items.
type ContextPackOutput struct {
	Items []ContextItem
}
