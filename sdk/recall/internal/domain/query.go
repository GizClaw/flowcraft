package domain

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

// TimeRangeFrom is a convenience for building a half-open range.
func TimeRangeFrom(from, to time.Time) TimeRange {
	return TimeRange{From: from, To: to}
}

// Query is the v2 recall input shape. Structured hints activate
// optional sources (timeline / relation / profile) via the planner;
// omitting them preserves PR-3 retrieval+entity behaviour.
type Query struct {
	Text      string
	Entities  []string
	Limit     int
	Subject   string
	Predicate string
	Object    string
	Kinds     []FactKind
	TimeRange TimeRange
	// GraphHops bounds graph expansion when graph is enabled via
	// WithGraphEnabled. Zero uses the graph projection default.
	GraphHops int

	// Trust applies read-time visibility filtering (Phase D.2). Nil
	// disables the trust_filter stage.
	Trust *TrustContext

	// IncludeRetired includes soft-closed and expired facts in recall
	// results (Phase D.8). Default false.
	IncludeRetired bool
}

// QueryIntent is the structured form of a caller Query after the
// query compiler and planner have interpreted it. The default query
// compiler is rule-based; an LLM query compiler is opt-in.
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

	// GraphEnabled is set by the planner when graph expansion is
	// wired and opted in at Memory construction (docs §17).
	GraphEnabled bool
	// GraphHops bounds BFS expansion; zero means the graph default.
	GraphHops int
}

// QueryPlan describes how the read pipeline will visit candidate
// sources for a single Recall call.
type QueryPlan struct {
	Intent        QueryIntent
	SourceOrder   []string
	SourceBudgets map[string]int
	TotalCap      int
}

// ContextItem is a materialized recall result. The Candidate field
// preserves the fusion provenance (score, source, rank) so explain
// traces and future ranking layers can use it.
type ContextItem struct {
	Candidate Candidate
	Fact      TemporalFact
	Evidence  []EvidenceRef
}

// Hit is one recall winner. Score is the fused score after the
// post-materialize ranker has applied its boost. Sources lists every
// CandidateSource that surfaced this fact, in the order fusion saw
// them; consumers can read it to attribute winners to specific
// sources (retrieval / entity / timeline / relation / profile /
// graph) for diagnostics and explainability. An empty Sources slice
// means the candidate carried no provenance metadata.
type Hit struct {
	Fact    TemporalFact
	Score   float64
	Sources []string
}
