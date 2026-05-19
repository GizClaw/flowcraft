package recall

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/projection"
)

// ProjectionRebuilder is the opt-in extension that exposes
// projection rebuild + targeted stale-id repair. Memory
// implementations that support reconcile can be type-asserted to
// this interface; callers that only do plain Save/Recall/Forget
// never see it.
//
// Semantics (docs §10.1):
//
//   - RebuildAll walks the canonical store with IncludeSuperseded=true
//     and re-projects every fact through fanout.Rebuild{Required,Optional}.
//     Memory does NOT pre-filter superseded facts — each projection
//     decides for itself how to materialize them, mirroring write-path
//     semantics. When an EvidenceStore is configured RebuildAll also
//     re-mirrors EvidenceRefs so the lookup adapter stays in sync.
//
//   - RebuildProjection targets one projection by name. Useful for
//     incident playbooks and tests; the projection name space is the
//     same one fanout / telemetry already use.
//
//   - RepairStale only forgets the listed fact ids from required +
//     optional projections. It deliberately does NOT touch the
//     canonical store and does NOT re-project anything. Operators
//     drive it from telemetry: pick up DriftStaleFact events,
//     batch by scope, hand the ids in.
//
// Neither method auto-repairs from Recall — drift attribution stays
// observable through the trace and telemetry, and repair is always
// explicit.
type ProjectionRebuilder interface {
	RebuildAll(ctx context.Context, scope Scope) error
	RebuildProjection(ctx context.Context, scope Scope, name string) error
	RepairStale(ctx context.Context, scope Scope, factIDs []string) error
}

// EvidenceLookup is the opt-in extension that exposes evidence
// retrieval by fact id. Implementations type-assert from Memory.
//
// The lookup prefers the secondary store when one is configured
// internally; without a store it falls back to the embedded
// TemporalFact.EvidenceRefs so callers always get a consistent view
// regardless of deployment topology.
type EvidenceLookup interface {
	GetEvidence(ctx context.Context, scope Scope, factID string) ([]EvidenceRef, error)
}

// DriftReason classifies a single drift observation surfaced by
// the read path. Aliases projection.DriftReason so the public
// surface and telemetry hook agree on the wire shape.
type DriftReason = projection.DriftReason

const (
	DriftStaleFact      = projection.DriftStaleFact
	DriftSupersededFact = projection.DriftSupersededFact
)

// DriftEvent is the public alias for projection.DriftEvent.
type DriftEvent = projection.DriftEvent

// ProjectionEvent is the public alias for projection.ProjectionEvent.
type ProjectionEvent = projection.ProjectionEvent

// TelemetryHook is the public alias for projection.TelemetryHook.
// External telemetry adapters implement OnProjection + OnDrift to
// receive every fanout outcome plus every materialize drift drop.
type TelemetryHook = projection.TelemetryHook
