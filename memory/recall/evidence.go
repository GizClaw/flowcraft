package recall

import (
	"context"

	evidencestore "github.com/GizClaw/flowcraft/memory/recall/internal/store/evidence"
)

// EvidenceStore is the optional secondary lookup adapter for evidence
// refs. Embedded TemporalFact.EvidenceRefs remain authoritative; this
// store exists for fast UI/eval/audit lookup and is rebuildable from
// canonical facts via ProjectionRebuilder.RebuildAll.
type EvidenceStore interface {
	Append(ctx context.Context, scope Scope, factID string, refs []EvidenceRef) error
	// Get returns a ref by evidence id only when that id is unique in scope.
	// Shared turn/evidence ids across facts are ambiguous; use ListByFact for
	// fact-scoped lookup.
	Get(ctx context.Context, scope Scope, evidenceID string) (EvidenceRef, error)
	ListByFact(ctx context.Context, scope Scope, factID string) ([]EvidenceRef, error)
	ListFactIDs(ctx context.Context, scope Scope) ([]string, error)
	ForgetByFact(ctx context.Context, scope Scope, factIDs []string) error
	Close() error
}

// NewMemoryEvidenceStore returns the in-memory EvidenceStore adapter
// shipped with sdk/recall. It is useful for local deployments, tests,
// and benchmarks that want secondary evidence lookup without wiring a
// durable backend.
func NewMemoryEvidenceStore() EvidenceStore {
	return evidencestore.NewMemoryStore()
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
