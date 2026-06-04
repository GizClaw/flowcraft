package recall

import "context"

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
