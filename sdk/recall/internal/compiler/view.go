package compiler

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// View is the minimal read-only surface that ConflictResolver
// requires from the canonical store. Keeping it narrow lets the
// compiler stay free of mutation side-effects: it can inspect the
// ledger to make merge / supersede decisions, but it never writes.
//
// Implementations adapt a temporal store; tests can satisfy the
// interface with a static in-memory map.
type View interface {
	// FindByMergeKey returns every canonical fact in the scope
	// sharing mergeKey, ordered by ObservedAt ascending. Empty
	// mergeKey returns an empty slice.
	FindByMergeKey(ctx context.Context, scope model.Scope, mergeKey string) ([]model.TemporalFact, error)

	// Get returns a fact by id within scope, or the store's
	// ErrNotFound when missing. ConflictResolver uses it to fetch
	// content for hashed-dedupe of free-form notes.
	Get(ctx context.Context, scope model.Scope, factID string) (model.TemporalFact, error)
}

// emptyView is the View used when callers do not wire a store. It
// makes every fact look brand new.
type emptyView struct{}

func (emptyView) FindByMergeKey(context.Context, model.Scope, string) ([]model.TemporalFact, error) {
	return nil, nil
}
func (emptyView) Get(context.Context, model.Scope, string) (model.TemporalFact, error) {
	return model.TemporalFact{}, ErrNotInView
}

// ErrNotInView signals that emptyView (or a similar adapter) has no
// data for the requested fact id. ConflictResolver treats it the
// same as "fact does not exist" rather than propagating it as an
// error.
var ErrNotInView = errSentinel("recall compiler view: not available")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }
