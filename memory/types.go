package memory

import (
	internalexecutor "github.com/GizClaw/flowcraft/memory/internal/executor"
	"github.com/GizClaw/flowcraft/memory/views"
	viewrecent "github.com/GizClaw/flowcraft/memory/views/recent"
)

// Public result DTOs and context item types are aliases to executor DTOs. Keep
// these aliases centralized here so facade/control-plane types stay separate.
type (
	DocumentChunkSearchResponse  = internalexecutor.DocumentChunkSearchResponse
	DocumentChunkSearchHit       = internalexecutor.DocumentChunkSearchHit
	SummaryNodeSearchResponse    = internalexecutor.SummaryNodeSearchResponse
	SummaryNodeSearchHit         = internalexecutor.SummaryNodeSearchHit
	ObservationSearchResponse    = internalexecutor.ObservationSearchResponse
	ObservationSearchHit         = internalexecutor.ObservationSearchHit
	FactSearchResponse           = internalexecutor.FactSearchResponse
	FactSearchHit                = internalexecutor.FactSearchHit
	FactGraphBuildResult         = internalexecutor.FactGraphBuildResult
	FactGraphSearchResponse      = internalexecutor.FactGraphSearchResponse
	FactGraphSearchHit           = internalexecutor.FactGraphSearchHit
	EntityProfileSearchResponse  = internalexecutor.EntityProfileSearchResponse
	EntityProfileSearchHit       = internalexecutor.EntityProfileSearchHit
	EntityTimelineSearchResponse = internalexecutor.EntityTimelineSearchResponse
	EntityTimelineSearchHit      = internalexecutor.EntityTimelineSearchHit
	ContextPack                  = internalexecutor.ContextPack
	ContextItemKind              = internalexecutor.ContextItemKind
	ContextItem                  = internalexecutor.ContextItem
	Scope                        = views.Scope
)

// ContextPackInput carries deterministic candidate evidence for a ContextPacker.
// It intentionally exposes product scope, query, window, items, and typed hits,
// but not physical retrieval namespaces or executor requests.
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

// ContextPackOutput contains the final items selected by a ContextPacker.
// An empty or nil Items slice is valid and filters all context items.
type ContextPackOutput struct {
	Items []ContextItem
}

const (
	ContextItemRecentMessage  ContextItemKind = internalexecutor.ContextItemRecentMessage
	ContextItemSummaryNode    ContextItemKind = internalexecutor.ContextItemSummaryNode
	ContextItemDocumentChunk  ContextItemKind = internalexecutor.ContextItemDocumentChunk
	ContextItemObservation    ContextItemKind = internalexecutor.ContextItemObservation
	ContextItemFact           ContextItemKind = internalexecutor.ContextItemFact
	ContextItemFactGraphNode  ContextItemKind = internalexecutor.ContextItemFactGraphNode
	ContextItemFactGraphEdge  ContextItemKind = internalexecutor.ContextItemFactGraphEdge
	ContextItemEntityProfile  ContextItemKind = internalexecutor.ContextItemEntityProfile
	ContextItemEntityTimeline ContextItemKind = internalexecutor.ContextItemEntityTimeline
)
