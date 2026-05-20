package domain

// Scope identifies the tenant/user partition for canonical memory.
//
// RuntimeID and UserID participate in storage / namespace partitioning;
// AgentID is a soft-isolation dimension surfaced through metadata, not
// through partitioning, so a single agent can union its own facts with
// shared ones during recall.
type Scope struct {
	RuntimeID string
	AgentID   string
	UserID    string
}

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
