package governance

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

// Governance bundles write-path policy hooks (docs §10.2). All
// defaults are no-op so Save never blocks unless callers opt in.
type Governance struct {
	Write       WritePolicy
	Retention   RetentionPolicy
	Sensitivity SensitivityPolicy
}

// Default returns audit-only no-op governance.
func Default() Governance {
	return Governance{
		Write:       NopWritePolicy{},
		Retention:   NopRetention{},
		Sensitivity: NopSensitivity{},
	}
}

// ApplyWrite runs write-path governance in deterministic order:
// sensitivity -> retention -> write policy.
func (g Governance) ApplyWrite(ctx context.Context, scope domain.Scope, f domain.TemporalFact, now time.Time) (domain.TemporalFact, bool) {
	if g.Sensitivity == nil {
		g.Sensitivity = NopSensitivity{}
	}
	if g.Retention == nil {
		g.Retention = NopRetention{}
	}
	if g.Write == nil {
		g.Write = NopWritePolicy{}
	}
	f, ok := g.Sensitivity.Apply(ctx, scope, f)
	if !ok {
		return f, false
	}
	if !g.Retention.Allow(ctx, scope, f, now) {
		return f, false
	}
	return g.Write.Apply(f)
}
