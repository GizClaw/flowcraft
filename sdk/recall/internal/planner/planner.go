// Package planner turns a caller Query into a deterministic
// QueryPlan. PR-3 ships a rule-based planner; the boundary keeps
// shape stable for an opt-in LLM intent parser in later phases
// (docs §9.1).
package planner

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// Source identifiers. Declare new sources here alongside their
// implementation so budgets and fusion weights stay aligned.
const (
	SourceRetrieval = "retrieval"
	SourceEntity    = "entity"
	SourceTimeline  = "timeline"
	SourceRelation  = "relation"
	SourceProfile   = "profile"
	SourceGraph     = "graph"
)

// Default per-source RRF weights (docs §9.3 / PR-6).
const (
	WeightRetrieval = 1.0
	WeightRelation  = 0.9
	WeightProfile   = 0.9
	WeightEntity    = 0.8
	WeightTimeline  = 0.7
	WeightGraph     = 0.75
)

// DefaultLimit applies when a caller leaves Query.Limit == 0.
const DefaultLimit = 10

// MaxLimit is the hard cap on returned hits.
const MaxLimit = 100

// SourceOverfetchMultiplier controls per-source candidate budgets.
// QueryPlan.TotalCap remains the final hit cap; source budgets are
// intentionally larger so fusion/ranking can choose from a broader
// candidate pool.
const SourceOverfetchMultiplier = 2

// MaxSourceOverfetch caps individual source budgets to keep broad
// multi-source reads bounded.
const MaxSourceOverfetch = 50

// Input is the planner contract.
type Input struct {
	Scope        model.Scope
	Text         string
	Entities     []string
	Subject      string
	Predicate    string
	Object       string
	Kinds        []model.FactKind
	TimeRange    model.TimeRange
	Limit        int
	GraphEnabled bool
	GraphHops    int
}

// Planner produces a QueryPlan.
type Planner interface {
	Plan(ctx context.Context, input Input) (model.QueryPlan, error)
}

// RuleBased is the deterministic planner.
type RuleBased struct {
	// RetrievalShare applies only when retrieval+entity are the sole
	// active sources (PR-3 behaviour). When structured sources are
	// also active budgets are weight-normalized instead.
	RetrievalShare float64
	// GraphEnabled opts into the graph source (docs §17 default off).
	GraphEnabled bool
}

// New returns the default rule-based planner.
func New() *RuleBased { return &RuleBased{RetrievalShare: 0.6} }

// Plan returns the QueryPlan with normalized limits and per-source
// budgets.
func (r *RuleBased) Plan(_ context.Context, input Input) (model.QueryPlan, error) {
	limit := input.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}

	intent := model.QueryIntent{
		Text:         input.Text,
		Entities:     input.Entities,
		Subject:      input.Subject,
		Predicate:    input.Predicate,
		Object:       input.Object,
		Kinds:        append([]model.FactKind(nil), input.Kinds...),
		TimeRange:    input.TimeRange,
		Scope:        input.Scope,
		Limit:        limit,
		GraphEnabled: input.GraphEnabled && r.GraphEnabled,
		GraphHops:    input.GraphHops,
	}

	order := buildSourceOrder(intent)
	budgets := allocateBudgets(order, limit)

	return model.QueryPlan{
		Intent:        intent,
		SourceOrder:   order,
		SourceBudgets: budgets,
		TotalCap:      limit,
	}, nil
}

// ActivatesTimeline reports whether the timeline source should run.
func ActivatesTimeline(intent model.QueryIntent) bool {
	if !intent.TimeRange.IsZero() {
		return true
	}
	return kindsIntersectTimeline(intent.Kinds)
}

// ActivatesRelation reports whether the relation source should run.
func ActivatesRelation(intent model.QueryIntent) bool {
	return intent.Subject != "" || intent.Predicate != "" || intent.Object != ""
}

// ActivatesProfile reports whether the profile source should run.
func ActivatesProfile(intent model.QueryIntent) bool {
	return intent.Subject != ""
}

// ActivatesGraph reports whether bounded graph expansion should run.
func ActivatesGraph(intent model.QueryIntent) bool {
	return intent.GraphEnabled && len(intent.Entities) > 0
}

func (r *RuleBased) retrievalEntityOnly(intent model.QueryIntent) bool {
	if ActivatesTimeline(intent) || ActivatesRelation(intent) || ActivatesProfile(intent) || ActivatesGraph(intent) {
		return false
	}
	return true
}

func buildSourceOrder(intent model.QueryIntent) []string {
	order := []string{SourceRetrieval}
	if len(intent.Entities) > 0 {
		order = append(order, SourceEntity)
	}
	if ActivatesGraph(intent) {
		order = append(order, SourceGraph)
	}
	if ActivatesRelation(intent) {
		order = append(order, SourceRelation)
	}
	if ActivatesProfile(intent) {
		order = append(order, SourceProfile)
	}
	if ActivatesTimeline(intent) {
		order = append(order, SourceTimeline)
	}
	return order
}

func kindsIntersectTimeline(kinds []model.FactKind) bool {
	if len(kinds) == 0 {
		return false
	}
	for _, k := range kinds {
		switch k {
		case model.KindEvent, model.KindPlan, model.KindState:
			return true
		}
	}
	return false
}

// allocateBudgets splits limit across active sources. When only
// retrieval+entity are active the PR-3 RetrievalShare split applies.
// Otherwise budgets are weight-normalized to sum to limit. When
// limit < len(sources) the first limit sources each get budget 1.
func allocateBudgets(order []string, limit int) map[string]int {
	budgets := make(map[string]int, len(order))
	if len(order) == 0 {
		return budgets
	}
	b := sourceBudget(limit)
	for _, src := range order {
		budgets[src] = b
	}
	return budgets
}

func sourceBudget(limit int) int {
	if limit <= 0 {
		limit = DefaultLimit
	}
	overfetch := limit * SourceOverfetchMultiplier
	if overfetch > MaxSourceOverfetch {
		overfetch = MaxSourceOverfetch
	}
	if overfetch < limit {
		overfetch = limit
	}
	if overfetch < 1 {
		overfetch = 1
	}
	return overfetch
}

func defaultWeights() map[string]float64 {
	return map[string]float64{
		SourceRetrieval: WeightRetrieval,
		SourceEntity:    WeightEntity,
		SourceRelation:  WeightRelation,
		SourceProfile:   WeightProfile,
		SourceTimeline:  WeightTimeline,
		SourceGraph:     WeightGraph,
	}
}

// prioritizeStructuredForTinyLimit ensures explicit subject/time/kind
// hints reach structured sources when limit cannot cover every source.
func prioritizeStructuredForTinyLimit(order []string, limit int) []string {
	var structured, rest []string
	for _, src := range order {
		switch src {
		case SourceTimeline, SourceRelation, SourceProfile, SourceGraph:
			structured = append(structured, src)
		default:
			rest = append(rest, src)
		}
	}
	merged := append(structured, rest...)
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
