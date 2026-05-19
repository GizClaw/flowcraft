package projection

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// IsSuperseded reports whether a fact has been replaced by another
// canonical write (CorrectedBy != ""). Per docs §5.4 ValidTo alone
// is not a supersede signal for temporal views.
func IsSuperseded(f model.TemporalFact) bool {
	return f.CorrectedBy != ""
}

// IsActive reports whether a fact belongs in an "active slot" view
// (profile / relation projections). Active means not superseded
// and either open-ended (ValidTo == nil) or still valid at now.
func IsActive(f model.TemporalFact, now time.Time) bool {
	if IsSuperseded(f) {
		return false
	}
	if f.ValidTo == nil {
		return true
	}
	return f.ValidTo.After(now)
}

// EffectiveTimestamp picks the sort/range key for timeline facts:
// ValidFrom when set, otherwise ObservedAt (docs §5.4).
func EffectiveTimestamp(f model.TemporalFact) time.Time {
	if f.ValidFrom != nil && !f.ValidFrom.IsZero() {
		return *f.ValidFrom
	}
	return f.ObservedAt
}
