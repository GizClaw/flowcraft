package projection

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// Consistency tells the fanout whether a projection failure must
// fail the write/forget call (Required) or only be logged (Optional).
type Consistency int

const (
	// Required projections gate Save / Forget success. Retrieval and
	// entity projections must always be Required so canonical writes
	// stay visible to downstream sources.
	Required Consistency = iota
	// Optional projections are best-effort; failures are logged via
	// the telemetry hook and do not abort the call.
	Optional
)

func (c Consistency) String() string {
	switch c {
	case Required:
		return "required"
	case Optional:
		return "optional"
	}
	return "unknown"
}

// Projection is a rebuildable derived view of the temporal ledger.
//
// Implementations must remain rebuildable from the canonical store
// alone; they must not become a truth layer. See docs §8.
type Projection interface {
	Name() string
	Consistency() Consistency
	Project(ctx context.Context, facts []model.TemporalFact) error
	Forget(ctx context.Context, scope model.Scope, factIDs []string) error
	Rebuild(ctx context.Context, scope model.Scope, facts []model.TemporalFact) error
}
