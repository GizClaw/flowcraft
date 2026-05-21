package diagnostic

// Types in this file are shared across multiple Detail records (or
// referenced from domain trace surfaces). They live in diagnostic/
// because they ARE the diagnostic vocabulary — fact / candidate
// drops, structurizer coverage tallies, activated lens descriptors.
//
// Cycle note: diagnostic/ deliberately does NOT import the parent
// domain/ package — the dependency goes the other way (domain
// imports diagnostic to embed StageDiagnostic on RecallTrace /
// SaveTrace). DroppedFact therefore carries the dropped fact as
// `any`; subsystem code that constructs it passes the concrete
// domain.TemporalFact value and read sites type-assert. Once Phase
// E.3 deletes the deprecated parallel observation channels we can
// revisit whether to introduce a forward-declared minimal Fact
// interface here.

// StructurizerCoverage tallies how many times each sub-task of the
// Structurizer actually filled a previously-empty field on its way
// through the ingest pipeline. Operators read this to attribute
// accuracy shifts to a specific Structurizer responsibility before
// reaching for the algorithm; e.g. if KindFilled stays at 0, the
// LLM's enum is doing all the classification work and the keyword
// fallback is dead code.
//
// TotalFactsSeen is the denominator every other counter rides on,
// so ratios stay meaningful when callers aggregate across runs.
type StructurizerCoverage struct {
	TotalFactsSeen      int
	KindFilled          int
	EntitiesFilled      int
	SubjectFilled       int
	ValidFromHintFilled int
}

// Add merges another coverage tally into this one. Used by the
// ingest pipeline to fold per-fact deltas into a single per-Save
// total.
func (c *StructurizerCoverage) Add(other StructurizerCoverage) {
	c.TotalFactsSeen += other.TotalFactsSeen
	c.KindFilled += other.KindFilled
	c.EntitiesFilled += other.EntitiesFilled
	c.SubjectFilled += other.SubjectFilled
	c.ValidFromHintFilled += other.ValidFromHintFilled
}

// FactStats tallies per-fact shape signals over a stage's output
// slice. Stages with access to domain.TemporalFact (Ingest, Resolve)
// precompute these counters before emitting their Detail so the
// diagnostic layer can render quality without re-walking facts —
// diagnostic/ cannot import domain/ (cycle), so the stage owns the
// translation.
//
// Counters use small-int semantics: a fact contributes to at most
// one of WithContent / StructuredOnly / EmptyRenderable so the three
// partition Total. WithEvidence / WithValidFrom / WithConfidence are
// independent presence flags. ByKind buckets by FactKind so operators
// can attribute drift to a specific kind (event/state/preference/…).
type FactStats struct {
	Total           int
	WithContent     int
	StructuredOnly  int
	EmptyRenderable int
	WithEvidence    int
	WithValidFrom   int
	WithConfidence  int
	ByKind          map[string]int
}

// DropReason categorises why a candidate did not survive read-path
// processing. Used by RecallTrace for failure attribution
// (docs §10.4).
type DropReason string

const (
	DropStaleFact      DropReason = "stale_fact"
	DropDuplicate      DropReason = "duplicate_fact_id"
	DropTotalCap       DropReason = "total_cap"
	DropPerSourceCap   DropReason = "per_source_cap"
	DropSuperseded     DropReason = "superseded"
	DropMaterializeErr DropReason = "materialize_error"
	// DropScopeViolation marks candidates whose canonical fact
	// lives outside the query scope's hard partition or violates
	// AgentID soft isolation. Materialization enforces this as a
	// defense-in-depth check after Sources, so third-party /
	// custom Sources cannot leak across tenants or agents.
	DropScopeViolation DropReason = "scope_violation"
	// DropRetired marks facts hidden by Closed or ExpiresAt (D.8).
	DropRetired DropReason = "retired"
)

// CandidateDrop records a single discarded candidate with its
// reason. Stage names ("fusion" / "materialize") let dashboards
// split drift sources.
type CandidateDrop struct {
	Stage   string
	Reason  DropReason
	FactID  string
	Source  string
	Details string
}

// DroppedFact carries a structured reason for why a candidate fact
// did not enter the canonical ledger.
//
// Fact is `any` to keep diagnostic/ a leaf (no domain import). In
// practice subsystems pass a domain.TemporalFact value; consumers
// type-assert before reading concrete fields. The public
// sdk/recall.DroppedFact surface narrows Fact back to the strongly-
// typed TemporalFact for caller ergonomics.
type DroppedFact struct {
	Fact   any
	Reason string
	// FactID / Kind / ContentHash are populated by ingest redaction for
	// default telemetry; Fact body is cleared to avoid PII retention.
	FactID      string
	Kind        string
	ContentHash string
}

// RedactDroppedFacts strips canonical fact payloads from drops so
// telemetry / traces do not retain PII after ForgetAll. Prefer
// ingest.RedactDroppedFacts when domain.TemporalFact values are
// available — it also fills FactID / Kind / ContentHash.
func RedactDroppedFacts(drops []DroppedFact) []DroppedFact {
	if len(drops) == 0 {
		return nil
	}
	out := make([]DroppedFact, len(drops))
	for i, d := range drops {
		out[i] = DroppedFact{
			Reason:      d.Reason,
			FactID:      d.FactID,
			Kind:        d.Kind,
			ContentHash: d.ContentHash,
		}
	}
	return out
}

// CompensationFailedDetail is the Detail the pipeline framework
// emits when a Stage's Compensator itself returns an error during
// rollback. The diagnostic carries Status=failed and Stage suffixed
// with ":compensate" so dashboards can distinguish a forward stage
// failure from a rollback failure on the same slot.
//
// OriginalStage names the stage whose compensator failed. Cause is
// the original Run error that triggered the rollback chain, so
// operators see both halves of the story in one event without
// cross-referencing trace.Stages.
type CompensationFailedDetail struct {
	OriginalStage string
	Cause         string
}

func (CompensationFailedDetail) isStageDetail() {}

// ActivatedLens is one row in PlanDetail.ActivatedLenses. Lives in
// shared.go (cross-detail vocabulary, per restructure §4).
type ActivatedLens struct {
	Lens        string
	Weight      float64
	Budget      int
	ActivatedBy string
}
