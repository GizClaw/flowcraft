package domain

import "time"

// FactVersion is one row in a fact's supersede history (Phase D.6).
// History walks the TemporalStore append-only chain; no journal store.
type FactVersion struct {
	Fact TemporalFact

	ValidFrom time.Time
	ValidTo   time.Time

	SupersededBy string
	Supersedes   []string
	Reason       string
}

// VersionFromFact maps a ledger fact into a FactVersion view.
func VersionFromFact(f TemporalFact) FactVersion {
	v := FactVersion{
		Fact:         f,
		SupersededBy: f.CorrectedBy,
		Supersedes:   append([]string(nil), f.Supersedes...),
	}
	if f.ValidFrom != nil && !f.ValidFrom.IsZero() {
		v.ValidFrom = *f.ValidFrom
	} else {
		v.ValidFrom = f.ObservedAt
	}
	if f.ValidTo != nil {
		v.ValidTo = *f.ValidTo
	}
	if rev, ok := RevisionOf(f); ok {
		v.Reason = string(rev.Kind)
	}
	return v
}
