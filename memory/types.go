package memory

import (
	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/memory/views/recent"
)

// ContextPack is the public facade result for composed read-time context.
type ContextPack struct {
	Window       recent.WindowResult
	MessageHits  []derive.SourceMessageSearchHit
	SummaryHits  []derive.SummaryNodeSearchHit
	DocumentHits []derive.DocumentChunkSearchHit
	EntityHits   []derive.EntityFactSearchHit
	Items        []derive.ContextItem
}

type Scope = views.Scope
