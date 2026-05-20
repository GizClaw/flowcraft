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
}
