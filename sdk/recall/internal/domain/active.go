package domain

import "time"

// IsSuperseded reports whether a fact has been replaced by another
// canonical write (CorrectedBy != ""). Per docs §5.4 ValidTo alone
// is not a supersede signal for temporal views.
func IsSuperseded(f TemporalFact) bool {
	return f.CorrectedBy != ""
}

// IsActive reports whether a fact belongs in an "active slot" view
// (profile / relation projections). Active means not superseded
// and either open-ended (ValidTo == nil) or still valid at now.
func IsActive(f TemporalFact, now time.Time) bool {
	if IsSuperseded(f) {
		return false
	}
	if f.ValidTo == nil {
		return true
	}
	return f.ValidTo.After(now)
}

// IsRetired reports whether a fact is hidden from default Recall:
// soft-closed or past ExpiresAt.
func IsRetired(f TemporalFact, now time.Time) bool {
	if f.Closed {
		return true
	}
	if f.ExpiresAt != nil && !f.ExpiresAt.IsZero() && !now.Before(*f.ExpiresAt) {
		return true
	}
	return false
}

// EffectiveTimestamp picks the sort/range key for timeline facts:
// ValidFrom when set, otherwise ObservedAt (docs §5.4).
func EffectiveTimestamp(f TemporalFact) time.Time {
	if f.ValidFrom != nil && !f.ValidFrom.IsZero() {
		return *f.ValidFrom
	}
	return f.ObservedAt
}
