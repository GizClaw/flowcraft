package stages

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/ingest"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
)

func droppedFactsForTelemetry(state *write.WriteState, drops []diagnostic.DroppedFact) []diagnostic.DroppedFact {
	if state != nil && state.DiagnosticsIncludeRaw {
		out := make([]diagnostic.DroppedFact, len(drops))
		copy(out, drops)
		return out
	}
	return ingest.RedactDroppedFacts(drops)
}
