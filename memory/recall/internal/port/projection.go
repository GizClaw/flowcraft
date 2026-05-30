package port

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

// Consistency tells the fanout whether a projection failure must
// fail the write/forget call (Required) or only be logged (Optional).
type Consistency int

const (
	// Required projections gate Save / Forget success. Retrieval
	// and entity projections must always be Required so canonical
	// writes stay visible to downstream sources.
	Required Consistency = iota
	// Optional projections are best-effort; failures are logged via
	// the telemetry hook and do not abort the call.
	Optional
)

// String returns the canonical name of the Consistency level.
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
// alone; they must not become a truth layer.
type Projection interface {
	Name() string
	Consistency() Consistency
	Project(ctx context.Context, facts []domain.TemporalFact) error
	Forget(ctx context.Context, scope domain.Scope, factIDs []string) error
	Rebuild(ctx context.Context, scope domain.Scope, facts []domain.TemporalFact) error
	// ClearScope removes every projection entry for a scope partition
	// without enumerating fact IDs. It backs Memory.ForgetAll and is
	// the only entry point with O(1)-per-projection semantics
	// instead of O(N) Forget. Implementations MUST be idempotent —
	// clearing an already-empty scope is a no-op.
	ClearScope(ctx context.Context, scope domain.Scope) error
}

// KindFilteredProjection is implemented by projections that opt out of
// (or opt into) specific FactKinds. The fanout uses this to route
// FactKind-restricted writes (e.g. KindEpisode only goes to evidence).
//
// Projections that don't implement this interface are treated as
// accepting every kind (current behaviour preserved).
type KindFilteredProjection interface {
	AcceptsKind(kind domain.FactKind) bool
}
