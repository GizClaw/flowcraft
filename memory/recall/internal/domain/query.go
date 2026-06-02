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

	// Trust applies read-time visibility filtering. Nil disables the
	// policy_filter stage.
	Trust *TrustContext

	// IncludeRetired includes soft-closed and expired facts in recall results.
	// Default false.
	IncludeRetired bool
}

// QueryIntent is the structured form of a caller Query after the intent router
// has selected a recall strategy and preserved low-risk literal signals.
type QueryIntent struct {
	Text      string
	Entities  []string
	Subject   string
	Predicate string
	Object    string
	Kinds     []FactKind
	TimeRange TimeRange
	Features  QueryFeatures
	Scope     Scope
	Limit     int
	Route     IntentRoute

	// GraphEnabled is set by the planner when graph expansion is
	// wired and opted in at Memory construction (docs §17).
	GraphEnabled bool
	// GraphHops bounds BFS expansion; zero means the graph default.
	GraphHops int
}

// QueryFeatures carries low-risk literal features produced during intent
// routing. These features may help retrieval and packing preserve exact
// evidence, but they should not be treated as a semantic intent classifier.
type QueryFeatures struct {
	Tokens  map[string]struct{}
	Numeric map[string]struct{}
	Quoted  map[string]struct{}
	Proper  map[string]struct{}

	Temporal QueryTemporalFeatures
}

// HasTimeSignal reports whether the query contains a literal time signal.
func (f QueryFeatures) HasTimeSignal() bool {
	return f.Temporal.HasExplicitDate ||
		f.Temporal.HasRelativeExpression ||
		!f.Temporal.TimeRange.IsZero()
}

// IsZero reports whether no query features were populated.
func (f QueryFeatures) IsZero() bool {
	return len(f.Tokens) == 0 &&
		len(f.Numeric) == 0 &&
		len(f.Quoted) == 0 &&
		len(f.Proper) == 0 &&
		!f.HasTimeSignal()
}

// QueryTemporalFeatures captures temporal literal signals.
type QueryTemporalFeatures struct {
	HasExplicitDate       bool
	HasRelativeExpression bool
	MatchedText           string
	TimeRange             TimeRange
}

type QueryTaskIntent string

const (
	QueryTaskDirectLookup      QueryTaskIntent = "direct_lookup"
	QueryTaskSetCompletion     QueryTaskIntent = "set_completion"
	QueryTaskBridgeResolution  QueryTaskIntent = "bridge_resolution"
	QueryTaskTemporalReasoning QueryTaskIntent = "temporal_reasoning"
	QueryTaskDisambiguation    QueryTaskIntent = "disambiguation"
	QueryTaskYesNoVerification QueryTaskIntent = "yes_no_verification"
	QueryTaskAbsenceCheck      QueryTaskIntent = "absence_check"
	QueryTaskCounterfactual    QueryTaskIntent = "counterfactual_check"
)

type RecallStrategy string

const (
	RecallStrategyDefault        RecallStrategy = "default"
	RecallStrategyTemporal       RecallStrategy = "temporal"
	RecallStrategySet            RecallStrategy = "set"
	RecallStrategyCount          RecallStrategy = "count"
	RecallStrategyJoin           RecallStrategy = "join"
	RecallStrategyIntersection   RecallStrategy = "intersection"
	RecallStrategyProfile        RecallStrategy = "profile"
	RecallStrategyYesNo          RecallStrategy = "yes_no"
	RecallStrategyCounterfactual RecallStrategy = "counterfactual"
)

type IntentRoute struct {
	Strategy       RecallStrategy
	Confidence     float64
	Alternates     []IntentRouteCandidate
	Signals        []string
	FallbackReason string
}

type IntentRouteCandidate struct {
	Strategy   RecallStrategy
	Confidence float64
}

func (r IntentRoute) EffectiveStrategy() RecallStrategy {
	if r.Strategy == "" {
		return RecallStrategyDefault
	}
	return r.Strategy
}

// QueryPlan describes how the read pipeline will visit candidate
// sources for a single Recall call.
//
// LensWeights carries optional, planner-derived per-lens weight hints for
// future structured planners. The current rule-based planner leaves it empty so
// entity snapshots do not change source ordering or ranking.
type QueryPlan struct {
	Intent        QueryIntent
	IntentRoute   IntentRoute
	SourceOrder   []string
	SourceBudgets map[string]int
	TotalCap      int
	LensWeights   map[string]float64
	TaskIntents   []QueryTaskIntent
}

// ContextItem is a materialized recall result. The Candidate field
// preserves the fusion provenance (score, source, rank) so explain
// traces and future ranking layers can use it.
type ContextItem struct {
	Candidate   Candidate
	Ref         CandidateRef
	Fact        TemporalFact
	Observation Observation
	Link        FactLink
	Evidence    []EvidenceRef
}

// Hit is one recall winner. Evidence is the grounded evidence slice exposed to
// consumers: candidate-matched refs come first, followed by bounded supporting
// refs from the same fact. Score is the fused score after deterministic rank
// adjustments such as confidence, feedback, and decay. Sources lists every
// CandidateSource that surfaced this fact, in the order fusion saw them;
// consumers can read it for diagnostics and explainability. An empty Sources
// slice means the candidate carried no provenance metadata.
type Hit struct {
	Ref            CandidateRef
	Fact           TemporalFact
	Observation    Observation
	Link           FactLink
	Evidence       []EvidenceRef
	EvidencePacket EvidencePacket
	Score          float64
	Sources        []string
	AnswerEvidence []EvidenceRow
}
