// Package port declares the contractual interfaces every recall
// subsystem depends on. It is the L0 leaf alongside domain — port
// can import domain but no subsystem; subsystems import port +
// domain; the public facade composes subsystems and the pipeline.
package port

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

// ListQuery filters scope-local List results. Empty fields are
// interpreted as "match anything" so callers can issue scope-wide
// scans by passing the zero value.
type ListQuery struct {
	Kinds             []domain.FactKind
	Entities          []string
	IncludeSuperseded bool
	Limit             int
}

// TemporalStore is the canonical TemporalFact ledger boundary.
//
// It is deliberately NOT a retrieval index: vector / BM25 search and
// fusion live in projections+sources, never inside the truth layer.
// Projection schema must not flow back into TemporalStore
// implementations.
type TemporalStore interface {
	// Append writes facts in order. Implementations enforce
	// append-first semantics: appending an updated value for an
	// existing merge_key MUST NOT mutate the prior fact's content,
	// only close its validity (see UpdateValidity).
	Append(ctx context.Context, facts []domain.TemporalFact) error

	// Get returns a single fact by scope-qualified ID. Returns
	// the store's ErrNotFound when missing.
	Get(ctx context.Context, scope domain.Scope, factID string) (domain.TemporalFact, error)

	// List returns scope-local facts matching the query, ordered by
	// ObservedAt ascending.
	List(ctx context.Context, scope domain.Scope, query ListQuery) ([]domain.TemporalFact, error)

	// FindByMergeKey returns all facts in the scope sharing a
	// merge_key, ordered by ObservedAt ascending.
	FindByMergeKey(ctx context.Context, scope domain.Scope, mergeKey string) ([]domain.TemporalFact, error)

	// FindSupersededBy returns facts whose CorrectedBy equals the
	// supplied factID. Used by reconcile and audit flows.
	FindSupersededBy(ctx context.Context, scope domain.Scope, factID string) ([]domain.TemporalFact, error)

	// FindByRevisionSource returns every fact whose Revision
	// metadata (see domain.RevisionOf) points back at sourceFactID
	// via SourceFactID — i.e. forks, contests, and (future) merges
	// anchored on that root. Used by Memory.Lineage to walk the
	// revision DAG outward from a query fact.
	FindByRevisionSource(ctx context.Context, scope domain.Scope, sourceFactID string) ([]domain.TemporalFact, error)

	// UpdateValidity closes a fact's validity window. Idempotent:
	// supplying the same validTo+correctedBy on an already-closed
	// fact is a no-op rather than an error.
	UpdateValidity(ctx context.Context, scope domain.Scope, factID string, validTo time.Time, correctedBy string) error

	// ReopenValidity is the deliberate inverse of UpdateValidity
	// used by Save-rollback compensation. It clears ValidTo and
	// CorrectedBy on factID — but only when the current CorrectedBy
	// equals expectedCorrectedBy.
	ReopenValidity(ctx context.Context, scope domain.Scope, factID string, expectedCorrectedBy string) error

	// Delete removes facts by ID within a scope. Missing IDs are
	// ignored so callers can issue idempotent forgets.
	Delete(ctx context.Context, scope domain.Scope, factIDs []string) error

	// UpdateFeedback adjusts reinforcement and penalty on an existing
	// fact (Phase D.4). Deltas are added then clamped to >= 0.
	UpdateFeedback(ctx context.Context, scope domain.Scope, factID string, reinforcementDelta, penaltyDelta float64) error

	// MarkClosed sets or clears the soft-delete flag (Phase D.8).
	MarkClosed(ctx context.Context, scope domain.Scope, factID string, closed bool) error

	// ListByID returns every fact in the scope's supersede chain for
	// factID, ObservedAt ascending (Phase D.6 History view).
	ListByID(ctx context.Context, scope domain.Scope, factID string) ([]domain.TemporalFact, error)

	// DeleteByScope removes every fact in the scope partition (Phase
	// D.8 ForgetAll Hard). Returns the number of facts removed.
	DeleteByScope(ctx context.Context, scope domain.Scope) (int, error)

	// Close releases backend resources.
	Close() error
}

// EvidenceStore is the secondary evidence lookup adapter.
//
// It is fed by Memory mirror-writing TemporalFact.EvidenceRefs after
// a successful TemporalStore.Append, and is queried by EvidenceLookup
// callers. The adapter never holds anything not derivable from
// canonical facts.
type EvidenceStore interface {
	// Append attaches refs to factID within scope. Implementations
	// must be idempotent on (scope, factID, refs[i].ID).
	Append(ctx context.Context, scope domain.Scope, factID string, refs []domain.EvidenceRef) error

	// Get returns one EvidenceRef by id. Missing → ErrNotFound.
	Get(ctx context.Context, scope domain.Scope, evidenceID string) (domain.EvidenceRef, error)

	// ListByFact returns refs in append order.
	ListByFact(ctx context.Context, scope domain.Scope, factID string) ([]domain.EvidenceRef, error)

	// ListFactIDs enumerates every fact id that has at least one
	// evidence ref recorded in this scope.
	ListFactIDs(ctx context.Context, scope domain.Scope) ([]string, error)

	// ForgetByFact removes all refs attached to the listed fact
	// ids. Missing ids are tolerated so callers can issue
	// idempotent forgets after partial failures.
	ForgetByFact(ctx context.Context, scope domain.Scope, factIDs []string) error

	// Close releases backend resources.
	Close() error
}
