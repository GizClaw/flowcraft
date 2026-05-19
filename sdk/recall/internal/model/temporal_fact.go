package model

import "time"

type Scope struct {
	RuntimeID string
	AgentID   string
	UserID    string
}

type FactKind string

const (
	KindEvent      FactKind = "event"
	KindState      FactKind = "state"
	KindPreference FactKind = "preference"
	KindRelation   FactKind = "relation"
	KindPlan       FactKind = "plan"
	KindNote       FactKind = "note"
)

type EvidenceRef struct {
	ID        string
	MessageID string
	Role      string
	Text      string
	Timestamp time.Time
}

type TemporalFact struct {
	ID      string
	Scope   Scope
	Kind    FactKind
	Content string

	Subject   string
	Predicate string
	Object    string

	Entities     []string
	Participants []string

	ObservedAt time.Time
	ValidFrom  *time.Time
	ValidTo    *time.Time

	EvidenceRefs []EvidenceRef
	Confidence   float64
	Metadata     map[string]any
}
