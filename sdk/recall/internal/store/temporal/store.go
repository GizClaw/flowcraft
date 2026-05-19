package temporal

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// ErrNotFound is returned by Get / UpdateValidity / ReopenValidity
// when the fact does not exist in the requested scope.
//
// Classified as errdefs.NotFound so the public boundary
// (sdk/recall.Memory) and HTTP shims map it to 404 without each
// caller re-checking message text. The sentinel identity is
// preserved via the wrapped inner error so existing
// errors.Is(err, ErrNotFound) checks keep working.
var ErrNotFound = errdefs.NotFound(errdefs.New("recall temporal store: fact not found"))

// ErrReopenConflict is returned by ReopenValidity when the fact's
// current CorrectedBy does not match the expected value supplied by
// the caller. This means another writer has legitimately closed the
// fact for a different reason and rollback must NOT clobber it.
//
// Classified as errdefs.Conflict so callers can distinguish the
// "guard failed, do not retry" case from a transient store error.
var ErrReopenConflict = errdefs.Conflict(errdefs.New("recall temporal store: reopen guard mismatch"))

// ErrValidityAlreadyClosed is returned by UpdateValidity when the
// target fact already carries a non-zero ValidTo and the caller
// supplies a (validTo, correctedBy) tuple that does not match the
// existing one. The store stays strict so callers that DO require
// exclusive close semantics (e.g. RebuildAll) see the conflict; the
// canonical Save pipeline treats it as a benign race signal because
// the desired post-state ("prior fact is closed") is already true.
//
// Classified as errdefs.Conflict so it remains a 409-shaped failure
// at the public boundary for any caller that has not opted in to the
// tolerant interpretation.
var ErrValidityAlreadyClosed = errdefs.Conflict(errdefs.New("recall temporal store: fact validity already closed"))

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

	// ReopenValidity is the deliberate inverse of UpdateValidity
	// used by Save-rollback compensation. It clears ValidTo and
	// CorrectedBy on factID — but only when the current CorrectedBy
	// equals expectedCorrectedBy. The guard prevents rollback from
	// silently reopening a fact that some other write has since
	// closed for a legitimate reason.
	//
	// Returns ErrNotFound when the fact is missing and
	// ErrReopenConflict when the guard fails.
	ReopenValidity(ctx context.Context, scope model.Scope, factID string, expectedCorrectedBy string) error

	// Delete removes facts by ID within a scope. Missing IDs are
	// ignored so callers can issue idempotent forgets.
	Delete(ctx context.Context, scope model.Scope, factIDs []string) error

	// Close releases backend resources.
	Close() error
}
