package memory

import (
	internalexecutor "github.com/GizClaw/flowcraft/memory/internal/executor"
	"github.com/GizClaw/flowcraft/memory/views"
)

// Public result DTOs and context item types are aliases to executor DTOs. Keep
// these aliases centralized here so facade/control-plane types stay separate.
type (
	DocumentChunkSearchResponse = internalexecutor.DocumentChunkSearchResponse
	DocumentChunkSearchHit      = internalexecutor.DocumentChunkSearchHit
	SummaryNodeSearchResponse   = internalexecutor.SummaryNodeSearchResponse
	SummaryNodeSearchHit        = internalexecutor.SummaryNodeSearchHit
	ObservationSearchResponse   = internalexecutor.ObservationSearchResponse
	ObservationSearchHit        = internalexecutor.ObservationSearchHit
	FactSearchResponse          = internalexecutor.FactSearchResponse
	FactSearchHit               = internalexecutor.FactSearchHit
	FactGraphBuildResult        = internalexecutor.FactGraphBuildResult
	FactGraphSearchResponse     = internalexecutor.FactGraphSearchResponse
	FactGraphSearchHit          = internalexecutor.FactGraphSearchHit
	ContextPack                 = internalexecutor.ContextPack
	ContextItemKind             = internalexecutor.ContextItemKind
	ContextItem                 = internalexecutor.ContextItem
	Scope                       = views.Scope
)

const (
	ContextItemRecentMessage ContextItemKind = internalexecutor.ContextItemRecentMessage
	ContextItemSummaryNode   ContextItemKind = internalexecutor.ContextItemSummaryNode
	ContextItemDocumentChunk ContextItemKind = internalexecutor.ContextItemDocumentChunk
	ContextItemObservation   ContextItemKind = internalexecutor.ContextItemObservation
	ContextItemFact          ContextItemKind = internalexecutor.ContextItemFact
	ContextItemFactGraphNode ContextItemKind = internalexecutor.ContextItemFactGraphNode
	ContextItemFactGraphEdge ContextItemKind = internalexecutor.ContextItemFactGraphEdge
)
