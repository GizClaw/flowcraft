package domain

import (
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

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

	// Closed marks a soft-forgotten fact (Phase D.8). Soft Forget sets
	// Closed=true without deleting the ledger row; default Recall hides
	// closed facts unless Query.IncludeRetired is set.
	Closed bool

	// ExpiresAt is an optional TTL; facts past this instant are treated
	// as retired on read unless IncludeRetired is set.
	ExpiresAt *time.Time

	// Origin is the durable-work-item idempotency identifier (Phase
	// F.1). Zero value for pre-Origin facts and synchronous direct
	// writes. Set by the episode lane (OriginKindEpisode) and the
	// async semantic worker (OriginKindSemanticDerivation); see
	// recall-v2-async-semantic-write.md §3.3.
	Origin FactOrigin

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
	out.Origin.EpisodeFactIDs = cloneStrings(f.Origin.EpisodeFactIDs)
	out.Metadata = cloneMetadata(f.Metadata)
	if f.ValidFrom != nil {
		v := *f.ValidFrom
		out.ValidFrom = &v
	}
	if f.ValidTo != nil {
		v := *f.ValidTo
		out.ValidTo = &v
	}
	if f.ExpiresAt != nil {
		v := *f.ExpiresAt
		out.ExpiresAt = &v
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

// IsSuperseded reports whether a fact has been replaced by another
// canonical write (CorrectedBy != ""). Per docs §5.4 ValidTo alone
// is not a supersede signal for temporal views.
func IsSuperseded(f TemporalFact) bool {
	return f.CorrectedBy != ""
}

// IsCanonicalActive reports whether a fact is canonically active in
// the ledger: not superseded by a successor (CorrectedBy == "") and
// either open-ended (ValidTo == nil) or still inside its validity
// window. It deliberately does NOT consider Closed (soft forget) or
// ExpiresAt (TTL) — those are read-time / projection concerns.
//
// This is the lowest of three layers used across recall (see also
// IsProjectable and the read-time IsRecallable in filterRetiredItems):
//
//   - canonical (this function): pure ledger truth — does a successor
//     exist? is the validity window still open? Used by store-level
//     invariants and history reconstruction.
//   - projectable (IsProjectable): the slice projections SHOULD index —
//     canonical AND not retired (not soft-closed, not TTL-expired).
//   - recallable (filterRetiredItems + trust): what default Recall
//     returns to a caller — projectable plus actor trust filtering.
func IsCanonicalActive(f TemporalFact, now time.Time) bool {
	if IsSuperseded(f) {
		return false
	}
	if f.ValidTo == nil {
		return true
	}
	return f.ValidTo.After(now)
}

// IsProjectable reports whether a fact belongs in projection indexes
// (profile / relation / graph / retrieval / entity / timeline). It
// is canonical-active AND not retired (Closed=false, ExpiresAt not
// past). Projections MUST use this rather than IsCanonicalActive so
// soft-forgotten or TTL-expired facts are dropped from their indexes
// — otherwise the projection caches drift away from the canonical
// truth until the next RebuildAll.
//
// See IsCanonicalActive for the three-layer model (canonical /
// projectable / recallable).
func IsProjectable(f TemporalFact, now time.Time) bool {
	return IsCanonicalActive(f, now) && !IsRetired(f, now)
}

// IsActive is a backward-compatibility alias for IsCanonicalActive.
//
// Deprecated: use IsCanonicalActive or IsProjectable; IsActive
// remains for v2 transition.
func IsActive(f TemporalFact, now time.Time) bool {
	return IsCanonicalActive(f, now)
}

// IsHistorical reports whether a fact belongs in HISTORICAL
// projection indexes (timeline / retrieval / entity / graph) — views
// that index the whole observed record, not just currently-active
// slots. It is intentionally LESS restrictive than IsProjectable:
// past-ValidTo facts remain indexable so "When did X happen?" style
// queries can still hit the underlying event. The predicate only
// drops a fact when it has been superseded by a successor (canonical
// truth lost) OR retired by soft-forget / TTL (operator opt-out).
//
// Cluster B (2026-05-21) initially routed every projection through
// IsProjectable, which conflates "currently-active state" with
// "indexable historical fact". The timeline docstring had always
// promised "a past event remains visible even when ValidTo is set —
// only CorrectedBy suppresses indexing"; this predicate restores
// that invariant for the four historical views while leaving the
// two active-slot views (profile / relation) on IsProjectable.
func IsHistorical(f TemporalFact, now time.Time) bool {
	return !IsSuperseded(f) && !IsRetired(f, now)
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

// Resolution is the output of the ConflictResolver. It separates two
// disjoint outcomes so the write pipeline can execute them
// transactionally:
//
//   - Facts: the facts that should be appended to the ledger verbatim.
//     Already includes any Supersedes pointers populated by the
//     resolver.
//   - Closes: previously-stored facts whose validity must be closed
//     after a successful Append. Each entry carries scope, fact id,
//     the ValidTo timestamp to write, and the new fact id that
//     supersedes it (becomes CorrectedBy).
//   - Drops: facts the resolver discarded (noop / dedupe), with a
//     structured reason for trace / telemetry.
type Resolution struct {
	Facts  []TemporalFact
	Closes []ValidityClose
	Drops  []diagnostic.DroppedFact
}

// ValidityClose instructs the write pipeline to close an existing
// fact's validity after the new facts have been appended.
type ValidityClose struct {
	Scope       Scope
	FactID      string
	ValidTo     time.Time
	CorrectedBy string
}
