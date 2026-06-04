package domain

// FactKind classifies a canonical memory fact. The enum is closed; see
// docs §5.3 for projection eligibility per kind.
type FactKind string

const (
	KindEvent      FactKind = "event"
	KindState      FactKind = "state"
	KindPreference FactKind = "preference"
	KindProcedure  FactKind = "procedure"
	KindRelation   FactKind = "relation"
	KindPlan       FactKind = "plan"
	KindNote       FactKind = "note"
	// KindParameter is a first-class slot/configuration assertion. Parameter
	// identity is derived from parameter metadata, not from natural-language
	// content.
	KindParameter FactKind = "parameter"
	// KindEpisode is the raw conversation episode captured by the
	// async semantic write lane. Episode facts represent durable
	// source turns, NOT semantic conclusions, and are excluded from
	// projections other than evidence (see
	// recall-v2-async-semantic-write.md §3.2).
	KindEpisode FactKind = "episode"
)

// IsValid reports whether k is one of the canonical FactKinds.
func (k FactKind) IsValid() bool {
	switch k {
	case KindEvent, KindState, KindPreference, KindProcedure, KindRelation, KindPlan, KindNote, KindParameter, KindEpisode:
		return true
	}
	return false
}
