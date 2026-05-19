package projection

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

type Consistency int

const (
	Required Consistency = iota
	Optional
)

// Projection is a rebuildable derived view of the temporal ledger.
type Projection interface {
	Name() string
	Consistency() Consistency
	Project(ctx context.Context, facts []model.TemporalFact) error
	Forget(ctx context.Context, scope model.Scope, factIDs []string) error
	Rebuild(ctx context.Context, scope model.Scope, facts []model.TemporalFact) error
}
