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
}

// TimeRangeFrom is a convenience for building a half-open range.
func TimeRangeFrom(from, to time.Time) TimeRange {
	return TimeRange{From: from, To: to}
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

// Hit is one recall winner. Score is the fused score after the
// post-materialize ranker has applied its boost. Sources lists every
// CandidateSource that surfaced this fact, in the order fusion saw
// them; consumers can read it to attribute winners to specific
// sources (retrieval / entity / timeline / relation / profile /
// graph) for diagnostics and explainability, or to weight downstream
// rendering by source provenance. An empty Sources slice means the
// candidate carried no provenance metadata (legacy / test-only
// paths); it does not imply the hit is invalid.
type Hit struct {
	Fact    TemporalFact
	Score   float64
	Sources []string
}
