package ingest

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// emptyView is the port.View used when callers do not wire a store.
// It makes every fact look brand new.
type emptyView struct{}

var _ port.View = emptyView{}

func (emptyView) FindByMergeKey(context.Context, domain.Scope, string) ([]domain.TemporalFact, error) {
	return nil, nil
}
func (emptyView) Get(context.Context, domain.Scope, string) (domain.TemporalFact, error) {
	return domain.TemporalFact{}, ErrNotInView
}

// ErrNotInView signals that emptyView (or a similar adapter) has no
// data for the requested fact id. ConflictResolver treats it the
// same as "fact does not exist" rather than propagating it as an
// error.
var ErrNotInView = errSentinel("recall ingest view: not available")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }
