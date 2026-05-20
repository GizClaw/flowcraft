package lens

import (
	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// Lens bundles one projection (write/rebuild) and optional read
// source for a single recall lens. Evidence is projection-only.
type Lens interface {
	Spec() planner.LensSpec
	Build(deps Deps) (Built, error)
}

// Deps are the shared backends memory wires at construction time.
type Deps struct {
	Store         port.TemporalStore
	EvidenceStore port.EvidenceStore
	Index         retrieval.Index
	Telemetry     port.TelemetryHook
	Embedder      embedding.Embedder
	GraphEnabled  bool
}

// Built is the wired runtime for one lens.
type Built struct {
	Projection port.Projection
	Source     port.Source
	EntitySnap port.EntitySnapshotter
}
