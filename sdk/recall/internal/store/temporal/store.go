package temporal

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

var ErrNotFound = errors.New("recall temporal store: fact not found")

type ListQuery struct {
	Kinds    []model.FactKind
	Entities []string
	Limit    int
}

// Store is the canonical TemporalFact ledger boundary. It is deliberately
// not a retrieval index.
type Store interface {
	Append(ctx context.Context, facts []model.TemporalFact) error
	Get(ctx context.Context, scope model.Scope, factID string) (model.TemporalFact, error)
	List(ctx context.Context, scope model.Scope, query ListQuery) ([]model.TemporalFact, error)
	Delete(ctx context.Context, scope model.Scope, factIDs []string) error
	Close() error
}
