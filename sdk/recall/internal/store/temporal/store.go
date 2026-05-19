package temporal

import (
	"context"
	"errors"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// ErrNotFound is returned by Get / UpdateValidity when the fact does
// not exist in the requested scope.
var ErrNotFound = errors.New("recall temporal store: fact not found")

// ListQuery filters scope-local List results. Empty fields are
// interpreted as "match anything" so callers can issue scope-wide
// scans by passing the zero value.
type ListQuery struct {
	// Kinds restricts results to the given canonical FactKinds.
	Kinds []model.FactKind
	// Entities requires the fact to mention every listed entity
	// (intersection, not union).
	Entities []string
	// IncludeSuperseded includes facts whose ValidTo is closed.
	// Default false hides historical revisions, matching the active
	// view most callers expect.
	IncludeSuperseded bool
	// Limit caps the number of returned facts; 0 means no cap.
	Limit int
}

// Store is the canonical TemporalFact ledger boundary.
//
// It is deliberately NOT a retrieval index: vector / BM25 search and
// fusion live in projections+sources, never inside the truth layer.
// Projection schema must not flow back into Store implementations.
type Store interface {
	// Append writes facts in order. Implementations enforce
	// append-first semantics: appending an updated value for an
	// existing merge_key MUST NOT mutate the prior fact's content,
	// only close its validity (see UpdateValidity).
	Append(ctx context.Context, facts []model.TemporalFact) error

	// Get returns a single fact by scope-qualified ID. Returns
	// ErrNotFound when missing.
	Get(ctx context.Context, scope model.Scope, factID string) (model.TemporalFact, error)

	// List returns scope-local facts matching the query, ordered by
	// ObservedAt ascending.
	List(ctx context.Context, scope model.Scope, query ListQuery) ([]model.TemporalFact, error)

	// FindByMergeKey returns all facts in the scope sharing a
	// merge_key, ordered by ObservedAt ascending. Empty mergeKey
	// returns an empty slice (never every fact).
	FindByMergeKey(ctx context.Context, scope model.Scope, mergeKey string) ([]model.TemporalFact, error)

	// FindSupersededBy returns facts whose CorrectedBy equals the
	// supplied factID. Used by reconcile and audit flows.
	FindSupersededBy(ctx context.Context, scope model.Scope, factID string) ([]model.TemporalFact, error)

	// UpdateValidity closes a fact's validity window. Idempotent:
	// supplying the same validTo+correctedBy on an already-closed
	// fact is a no-op rather than an error.
	UpdateValidity(ctx context.Context, scope model.Scope, factID string, validTo time.Time, correctedBy string) error

	// Delete removes facts by ID within a scope. Missing IDs are
	// ignored so callers can issue idempotent forgets.
	Delete(ctx context.Context, scope model.Scope, factIDs []string) error

	// Close releases backend resources.
	Close() error
}
