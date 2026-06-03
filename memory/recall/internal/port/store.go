// Package port declares the contractual interfaces every recall
// subsystem depends on. It is the L0 leaf alongside domain — port
// can import domain but no subsystem; subsystems import port +
// domain; the public facade composes subsystems and the pipeline.
package port

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
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

// ScopeListQuery filters store partitions for privileged/operator flows.
// Empty RuntimeID is implementation-defined; core helpers require RuntimeID
// so ordinary callers cannot accidentally sweep every tenant.
type ScopeListQuery struct {
	RuntimeID string
}

// ScopeEnumerator is an optional extension for stores that can enumerate
// canonical scope partitions. It is deliberately separate from TemporalStore
// so durable adapters can adopt it without breaking the base store contract.
type ScopeEnumerator interface {
	ListScopes(ctx context.Context, query ScopeListQuery) ([]domain.Scope, error)
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

	// FindByOriginRequestID returns every fact in scope whose
	// Origin.RequestID matches the given key. Used by the async
	// semantic worker to detect "facts already written by a prior
	// attempt" (retry idempotency).
	FindByOriginRequestID(ctx context.Context, scope domain.Scope, requestID string) ([]domain.TemporalFact, error)

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

	// UpdateFeedback adjusts reinforcement and penalty on an existing fact.
	// Deltas are added then clamped to >= 0.
	UpdateFeedback(ctx context.Context, scope domain.Scope, factID string, reinforcementDelta, penaltyDelta float64) error

	// MarkClosed sets or clears the soft-delete flag.
	MarkClosed(ctx context.Context, scope domain.Scope, factID string, closed bool) error

	// ListByID returns every fact in the scope's supersede chain for factID,
	// ObservedAt ascending.
	ListByID(ctx context.Context, scope domain.Scope, factID string) ([]domain.TemporalFact, error)

	// DeleteByScope removes every fact in the scope partition. Returns the
	// number of facts removed.
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

	// Get returns one EvidenceRef by id only when the id is unique in scope.
	// Shared turn/evidence ids across facts are ambiguous; use ListByFact for
	// fact-scoped lookup. Missing → ErrNotFound.
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

// ObservationListQuery filters scope-local ObservationStore.List results.
// Empty fields mean "match anything".
type ObservationListQuery struct {
	Kinds    []domain.ObservationKind
	SourceID string
	Limit    int
}

// ObservationStore is the canonical raw-evidence ledger boundary.
type ObservationStore interface {
	// Append writes observations in order. Implementations should be
	// idempotent for an already-present observation ID with identical content.
	Append(ctx context.Context, observations []domain.Observation) error

	Get(ctx context.Context, scope domain.Scope, observationID string) (domain.Observation, error)
	List(ctx context.Context, scope domain.Scope, query ObservationListQuery) ([]domain.Observation, error)

	// Delete removes observations by ID within a scope. Missing IDs are ignored
	// so write compensators can be idempotent.
	Delete(ctx context.Context, scope domain.Scope, observationIDs []string) error
	DeleteByScope(ctx context.Context, scope domain.Scope) (int, error)

	Close() error
}

// LinkListQuery filters scope-local LinkStore.List results. Empty fields mean
// "match anything".
type LinkListQuery struct {
	Types []domain.FactLinkType
	From  domain.GraphNodeRef
	To    domain.GraphNodeRef
	Limit int
}

// LinkStore is the canonical typed-edge ledger boundary.
type LinkStore interface {
	// Append writes links in order. MergeKey is the idempotency key: appending a
	// link with an already-seen MergeKey is a no-op.
	Append(ctx context.Context, links []domain.FactLink) error

	Get(ctx context.Context, scope domain.Scope, linkID string) (domain.FactLink, error)
	List(ctx context.Context, scope domain.Scope, query LinkListQuery) ([]domain.FactLink, error)
	FindByNode(ctx context.Context, scope domain.Scope, node domain.GraphNodeRef) ([]domain.FactLink, error)
	FindByMergeKey(ctx context.Context, scope domain.Scope, mergeKey string) ([]domain.FactLink, error)

	// Delete removes links by ID within a scope. Missing IDs are ignored so write
	// compensators can be idempotent.
	Delete(ctx context.Context, scope domain.Scope, linkIDs []string) error
	DeleteByNode(ctx context.Context, scope domain.Scope, node domain.GraphNodeRef) (int, error)
	DeleteByScope(ctx context.Context, scope domain.Scope) (int, error)

	Close() error
}
