package domain

// FactKind classifies a canonical memory fact. The enum is closed; see
// docs §5.3 for projection eligibility per kind.
type FactKind string

const (
	KindEvent      FactKind = "event"
	KindState      FactKind = "state"
	KindPreference FactKind = "preference"
	KindRelation   FactKind = "relation"
	KindPlan       FactKind = "plan"
	KindNote       FactKind = "note"
)

// IsValid reports whether k is one of the canonical FactKinds.
func (k FactKind) IsValid() bool {
	switch k {
	case KindEvent, KindState, KindPreference, KindRelation, KindPlan, KindNote:
		return true
	}
	return false
}
