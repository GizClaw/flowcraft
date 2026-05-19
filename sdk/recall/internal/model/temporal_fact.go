package model

import "time"

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

// EvidenceRef points back to source material used to produce a fact.
// Phase 1 keeps evidence embedded; a SourceEvidenceStore adapter lands
// in a later phase without breaking this shape.
type EvidenceRef struct {
	ID        string
	MessageID string
	Role      string
	Text      string
	Timestamp time.Time
}

// MergeHints are LLM-supplied hints about merge behaviour. They are
// schema-level metadata only and MUST NOT participate in canonical
// merge-key decisions (see docs §5.5).
type MergeHints struct {
	// SuggestedMergeKey is an opaque hint from upstream extractors.
	SuggestedMergeKey string
	// Supersedes lists fact IDs the upstream extractor believes are
	// replaced. The compiler treats these as hints, not authority.
	Supersedes []string
	// Extra carries arbitrary upstream notes; never read by canonical
	// merge logic.
	Extra map[string]any
}

// TemporalFact is the canonical write unit of v2 memory.
//
// All ledger writes go through this shape. Projections derive views
// from it; sources only emit candidates referencing it. Public
// sdk/recall re-exports it via type alias — this package is the single
// schema owner.
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

	Location string

	ObservedAt time.Time
	ValidFrom  *time.Time
	ValidTo    *time.Time

	EvidenceRefs     []EvidenceRef
	SourceMessageIDs []string
	EvidenceText     string

	Confidence float64

	MergeKey    string
	MergeHints  MergeHints
	Supersedes  []string
	CorrectedBy string

	Metadata map[string]any
}

// Clone returns a deep copy of the fact, safe for callers that mutate
// slices/maps after handing the fact off to canonical stores.
func (f TemporalFact) Clone() TemporalFact {
	out := f
	out.Entities = cloneStrings(f.Entities)
	out.Participants = cloneStrings(f.Participants)
	out.EvidenceRefs = cloneEvidence(f.EvidenceRefs)
	out.SourceMessageIDs = cloneStrings(f.SourceMessageIDs)
	out.Supersedes = cloneStrings(f.Supersedes)
	out.MergeHints = cloneMergeHints(f.MergeHints)
	out.Metadata = cloneMetadata(f.Metadata)
	if f.ValidFrom != nil {
		v := *f.ValidFrom
		out.ValidFrom = &v
	}
	if f.ValidTo != nil {
		v := *f.ValidTo
		out.ValidTo = &v
	}
	return out
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneEvidence(in []EvidenceRef) []EvidenceRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]EvidenceRef, len(in))
	copy(out, in)
	return out
}

func cloneMergeHints(in MergeHints) MergeHints {
	out := MergeHints{
		SuggestedMergeKey: in.SuggestedMergeKey,
		Supersedes:        cloneStrings(in.Supersedes),
	}
	if len(in.Extra) > 0 {
		out.Extra = make(map[string]any, len(in.Extra))
		for k, v := range in.Extra {
			out.Extra[k] = v
		}
	}
	return out
}

func cloneMetadata(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
