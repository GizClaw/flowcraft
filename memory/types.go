package memory

import (
	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/memory/views/fact"
	"github.com/GizClaw/flowcraft/memory/views/recent"
)

// FactGraphBuildResult contains the stored graph records produced from facts.
type FactGraphBuildResult struct {
	Nodes []fact.Node
	Edges []fact.Edge
}

// ContextPack is the public facade result for composed read-time context.
type ContextPack struct {
	Window             recent.WindowResult
	SummaryHits        []derive.SummaryNodeSearchHit
	DocumentHits       []derive.DocumentChunkSearchHit
	ObservationHits    []derive.ObservationSearchHit
	FactHits           []derive.FactSearchHit
	FactGraphHits      []derive.FactGraphSearchHit
	EntityProfileHits  []derive.EntityProfileSearchHit
	EntityTimelineHits []derive.EntityTimelineSearchHit
	Items              []derive.ContextItem
}

type Scope = views.Scope
