package domain

import "time"

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

	// Reinforcement and Penalty are caller feedback weights adjusted
	// via Memory.Reinforce / Penalize (Phase D.4). They influence
	// fusion and rank but are not part of merge-key identity.
	Reinforcement float64
	Penalty       float64

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
