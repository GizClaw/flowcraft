package model

import "time"

// TimeRange bounds timeline queries. Zero value means "no time
// filter" — both From and To unset. When only one bound is set the
// open end is unbounded on that side.
type TimeRange struct {
	From time.Time
	To   time.Time
}

// IsZero reports whether no time bounds were supplied.
func (tr TimeRange) IsZero() bool {
	return tr.From.IsZero() && tr.To.IsZero()
}

// QueryIntent is the structured form of a caller Query after the
// planner has interpreted it. PR-3 keeps this rule-based; an LLM
// intent parser is opt-in in later phases.
type QueryIntent struct {
	Text      string
	Entities  []string
	Subject   string
	Predicate string
	Object    string
	Kinds     []FactKind
	TimeRange TimeRange
	Scope     Scope
	Limit     int
}

// QueryPlan describes how the read pipeline will visit candidate
// sources for a single Recall call.
type QueryPlan struct {
	Intent        QueryIntent
	SourceOrder   []string
	SourceBudgets map[string]int
	TotalCap      int
}

// Candidate is the unit emitted by every CandidateSource. It is a
// pure pointer to a canonical fact + provenance: sources never
// materialize the fact itself (docs §9.2). EvidenceIDs survive into
// materialization so trace explanations can attribute hits.
type Candidate struct {
	FactID string
	Scope  Scope
	Source string
	Rank   int
	Score  float64

	EvidenceIDs []string
	Metadata    map[string]any
}

// SourceResult is one source's contribution to a query. Truncated
// signals the source hit its budget; Err carries non-fatal source
// failures so the fusion layer can degrade rather than abort.
type SourceResult struct {
	Source     string
	Candidates []Candidate
	Truncated  bool
	Err        error
	Latency    time.Duration
}

// DropReason categorises why a candidate did not survive read-path
// processing. Used by RecallTrace for failure attribution (docs §10.4).
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
// reason. Stage names "fusion" / "materialize" let dashboards split
// drift sources.
type CandidateDrop struct {
	Stage   string
	Reason  DropReason
	FactID  string
	Source  string
	Details string
}

// SourceTrace captures one source's execution metrics for explain.
type SourceTrace struct {
	Source    string
	Budget    int
	Returned  int
	Truncated bool
	Latency   time.Duration
	Err       string
}

// RecallTrace is the read-path failure attribution surface. It
// stays append-only / readable; v2 telemetry feeds off the same
// fields so explain traces and metrics share a schema.
type RecallTrace struct {
	Plan            QueryPlan
	Sources         []SourceTrace
	FusedCandidates int
	Drops           []CandidateDrop
	Materialized    int
	TotalLatency    time.Duration
}
