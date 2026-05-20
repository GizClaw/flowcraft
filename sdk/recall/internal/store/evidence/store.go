// Package evidence is the secondary-lookup boundary for raw source
// material attached to canonical facts.
//
// Per docs §7.2 evidence stays *embedded* on TemporalFact as the
// source of truth (EvidenceRefs / EvidenceText / SourceMessageIDs).
// This package provides a thin adapter so callers that need
// scope-keyed evidence lookup (UI surfaces, eval repair, audit
// trails) do not have to reload the whole canonical fact. The
// adapter MUST stay rebuildable from the canonical store — it
// never becomes a second truth layer.
package evidence

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

// ErrNotFound is returned by Get when the evidence id is missing
// in the requested scope.
//
// Classified as errdefs.NotFound so the public boundary maps to
// 404 without each caller pattern-matching the message; identity
// stays compatible with errors.Is(err, ErrNotFound).
var ErrNotFound = errdefs.NotFound(errdefs.New("recall evidence store: evidence not found"))

// Store is the secondary evidence lookup adapter.
//
// Semantics are deliberately narrow: the store is fed by Memory
// mirror-writing TemporalFact.EvidenceRefs after a successful
// store.Append, and is queried by EvidenceLookup callers. The
// store never holds anything not derivable from canonical facts;
// rebuild walks store.List(scope, IncludeSuperseded=true) and
// re-appends.
type Store interface {
	// Append attaches refs to factID within scope. Implementations
	// must be idempotent on (scope, factID, refs[i].ID): replaying
	// the same Append is a no-op, not a duplicate-id error, so
	// rebuild and rollback retries stay safe.
	//
	// Refs whose ID is empty are auto-assigned by the store using
	// "<factID>#<index>". Stable across replays so two Appends for
	// the same fact produce the same ids.
	Append(ctx context.Context, scope domain.Scope, factID string, refs []domain.EvidenceRef) error

	// Get returns one EvidenceRef by id. Missing → ErrNotFound.
	Get(ctx context.Context, scope domain.Scope, evidenceID string) (domain.EvidenceRef, error)

	// ListByFact returns refs in append order. Empty factID
	// returns an empty slice so callers cannot accidentally
	// enumerate the whole scope.
	ListByFact(ctx context.Context, scope domain.Scope, factID string) ([]domain.EvidenceRef, error)

	// ListFactIDs enumerates every fact id that has at least one
	// evidence ref recorded in this scope. Used by rebuild to
	// perform an exact-replace sweep: anything the adapter knows
	// about but the canonical store has dropped must be removed
	// so the adapter stays derivable from the ledger.
	//
	// Order is unspecified — callers MUST treat the result as a
	// set. Empty scope returns nil.
	ListFactIDs(ctx context.Context, scope domain.Scope) ([]string, error)

	// ForgetByFact removes all refs attached to the listed fact
	// ids. Missing ids are tolerated so callers can issue
	// idempotent forgets after partial failures.
	ForgetByFact(ctx context.Context, scope domain.Scope, factIDs []string) error

	// Close releases backend resources.
	Close() error
}
