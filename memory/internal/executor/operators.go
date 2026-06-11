package executor

import "github.com/GizClaw/flowcraft/memory/derive"

// Operator and hook DTOs are package-local aliases to the shared public
// contracts. This keeps executor call sites compact without duplicating fields.
type (
	DocumentChunker       = derive.DocumentChunker
	DocumentChunkInput    = derive.DocumentChunkInput
	Summarizer            = derive.Summarizer
	SummaryInput          = derive.SummaryInput
	ObservationExtractor  = derive.ObservationExtractor
	ObservationInput      = derive.ObservationInput
	FactReconciler        = derive.FactReconciler
	FactReconcileInput    = derive.FactReconcileInput
	FactGraphBuilder      = derive.FactGraphBuilder
	FactGraphInput        = derive.FactGraphInput
	FactGraphOutput       = derive.FactGraphOutput
	EntityProfileBuilder  = derive.EntityProfileBuilder
	EntityProfileInput    = derive.EntityProfileInput
	EntityTimelineBuilder = derive.EntityTimelineBuilder
	EntityTimelineInput   = derive.EntityTimelineInput
	ContextPacker         = derive.ContextPacker
	ContextPackInput      = derive.ContextPackInput
	ContextPackOutput     = derive.ContextPackOutput

	DocumentChunkSearchHit  = derive.DocumentChunkSearchHit
	SummaryNodeSearchHit    = derive.SummaryNodeSearchHit
	ObservationSearchHit    = derive.ObservationSearchHit
	FactSearchHit           = derive.FactSearchHit
	FactGraphSearchHit      = derive.FactGraphSearchHit
	EntityProfileSearchHit  = derive.EntityProfileSearchHit
	EntityTimelineSearchHit = derive.EntityTimelineSearchHit
	ContextItemKind         = derive.ContextItemKind
	ContextItem             = derive.ContextItem
)

const (
	ContextItemRecentMessage  ContextItemKind = derive.ContextItemRecentMessage
	ContextItemSummaryNode    ContextItemKind = derive.ContextItemSummaryNode
	ContextItemDocumentChunk  ContextItemKind = derive.ContextItemDocumentChunk
	ContextItemObservation    ContextItemKind = derive.ContextItemObservation
	ContextItemFact           ContextItemKind = derive.ContextItemFact
	ContextItemFactGraphNode  ContextItemKind = derive.ContextItemFactGraphNode
	ContextItemFactGraphEdge  ContextItemKind = derive.ContextItemFactGraphEdge
	ContextItemEntityProfile  ContextItemKind = derive.ContextItemEntityProfile
	ContextItemEntityTimeline ContextItemKind = derive.ContextItemEntityTimeline
)
