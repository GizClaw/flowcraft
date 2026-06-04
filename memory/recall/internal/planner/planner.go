// Package planner turns a caller Query into a deterministic
// QueryPlan. PR-3 ships a rule-based planner; the boundary keeps
// shape stable for an opt-in LLM intent parser in later phases
// (docs §9.1).
package planner

import (
	"context"
	"math"
	"slices"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// Source identifiers. Declare new sources here alongside their
// implementation so budgets and fusion weights stay aligned.
const (
	SourceRetrieval   = "retrieval"
	SourceEntity      = "entity"
	SourceTimeline    = "timeline"
	SourceRelation    = "relation"
	SourceProfile     = "profile"
	SourceGraph       = "graph"
	SourceAssertion   = "assertion"
	SourceObservation = "observation"
)

// Default per-source RRF weights (docs §9.3 / PR-6).
//
// Retrieval is the lexical anchor and the only lane expected to
// solo-win broadly. Other projections are candidate routes over the
// same canonical facts/observations; their weights are route priors,
// not independent evidence votes.
const (
	WeightRetrieval   = 1.0
	WeightTimeline    = 0.9
	WeightRelation    = 0.9
	WeightProfile     = 0.85
	WeightGraph       = 0.85
	WeightEntity      = 0.85
	WeightAssertion   = 0.95
	WeightObservation = 0.65
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

// FusionPoolMultiplier controls the cross-source pool size before the
// deterministic ranker and final selection trim to QueryPlan.TotalCap.
const FusionPoolMultiplier = 3

// MaxFusionCandidateCap caps the fused candidate pool. It is intentionally
// larger than MaxSourceOverfetch: each source remains bounded independently,
// while fusion can preserve more cross-source diversity for final selection.
const MaxFusionCandidateCap = 100

// RecallStrategyPlanner builds source order and budgets from explicit hints and
// the routed recall strategy.
type RecallStrategyPlanner struct {
	// Specs is the lens registration table (name, weight, activate).
	// When empty, Plan falls back to builtinSpecs().
	Specs []LensSpec
	// RetrievalShare applies only when retrieval+entity are the sole
	// active sources (PR-3 behaviour). When structured sources are
	// also active budgets are weight-normalized instead.
	RetrievalShare float64
	// GraphEnabled opts into the graph source (docs §17 default off).
	GraphEnabled bool
}

// New returns the default recall strategy planner with built-in lens
// specs (tests and callers that do not use lens.Registry).
func New() *RecallStrategyPlanner { return NewFromSpecs(builtinSpecs()) }

// NewFromSpecs constructs a planner driven by the supplied lens
// registration order and activation predicates.
func NewFromSpecs(specs []LensSpec) *RecallStrategyPlanner {
	return &RecallStrategyPlanner{Specs: specs, RetrievalShare: 0.6}
}

// builtinSpecs mirrors the default lens.Registry registration order
// and activation rules used by memory.New.
func builtinSpecs() []LensSpec {
	return []LensSpec{
		{Name: SourceRetrieval, Weight: WeightRetrieval, Activate: func(domain.QueryIntent) bool { return true }},
		{Name: SourceEntity, Weight: WeightEntity, Activate: func(i domain.QueryIntent) bool { return len(i.Entities) > 0 }},
		{Name: SourceGraph, Weight: WeightGraph, Activate: ActivatesGraph},
		{Name: SourceRelation, Weight: WeightRelation, Activate: ActivatesRelation},
		{Name: SourceAssertion, Weight: WeightAssertion, Activate: ActivatesAssertion},
		{Name: SourceProfile, Weight: WeightProfile, Activate: ActivatesProfile},
		{Name: SourceTimeline, Weight: WeightTimeline, Activate: ActivatesTimeline},
	}
}

var _ port.Planner = (*RecallStrategyPlanner)(nil)

// Plan returns the QueryPlan with normalized limits and per-source
// budgets.
func (r *RecallStrategyPlanner) Plan(_ context.Context, input port.PlannerInput) (domain.QueryPlan, error) {
	limit := input.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}

	intent := domain.QueryIntent{
		Text:         input.Text,
		Entities:     input.Entities,
		Subject:      input.Subject,
		Predicate:    input.Predicate,
		Object:       input.Object,
		Kinds:        append([]domain.FactKind(nil), input.Kinds...),
		TimeRange:    input.TimeRange,
		Features:     input.Features,
		Route:        input.IntentRoute,
		Scope:        input.Scope,
		Limit:        limit,
		GraphEnabled: input.GraphEnabled && r.GraphEnabled,
		GraphHops:    input.GraphHops,
	}

	order := r.buildSourceOrder(intent)
	weights := r.activeWeights(intent, order)
	budgets := allocateBudgets(order, limit, weights, r.RetrievalShare)

	return domain.QueryPlan{
		Intent:        intent,
		IntentRoute:   intent.Route,
		SourceOrder:   order,
		SourceBudgets: budgets,
		TotalCap:      limit,
		LensWeights:   weights,
		TaskIntents:   inferTaskIntents(intent),
	}, nil
}

func inferTaskIntents(intent domain.QueryIntent) []domain.QueryTaskIntent {
	var out []domain.QueryTaskIntent
	add := func(task domain.QueryTaskIntent) {
		if !slices.Contains(out, task) {
			out = append(out, task)
		}
	}
	switch intent.Route.EffectiveStrategy() {
	case domain.RecallStrategyTemporal:
		add(domain.QueryTaskTemporalReasoning)
	case domain.RecallStrategySet, domain.RecallStrategyCount:
		add(domain.QueryTaskSetCompletion)
	case domain.RecallStrategyJoin, domain.RecallStrategyIntersection:
		add(domain.QueryTaskBridgeResolution)
	case domain.RecallStrategyYesNo:
		add(domain.QueryTaskYesNoVerification)
	case domain.RecallStrategyCounterfactual:
		add(domain.QueryTaskCounterfactual)
	default:
		add(domain.QueryTaskDirectLookup)
	}
	return out
}

// ActivatesTimeline reports whether the timeline source should run.
func ActivatesTimeline(intent domain.QueryIntent) bool {
	if !intent.TimeRange.IsZero() {
		return true
	}
	return intent.Route.EffectiveStrategy() == domain.RecallStrategyTemporal
}

// ActivatesRelation reports whether the relation source should run.
func ActivatesRelation(intent domain.QueryIntent) bool {
	return intent.Subject != "" && (intent.Predicate != "" || intent.Object != "")
}

func ActivatesAssertion(intent domain.QueryIntent) bool {
	return intent.Subject != "" && intent.Predicate != "" && intent.Object != ""
}

// ActivatesProfile reports whether the profile source should run.
func ActivatesProfile(intent domain.QueryIntent) bool {
	if intent.Route.EffectiveStrategy() == domain.RecallStrategyProfile {
		return true
	}
	return intent.Subject != "" && kindsIntersectProfile(intent.Kinds)
}

// ActivatesGraph reports whether bounded graph expansion should run.
func ActivatesGraph(intent domain.QueryIntent) bool {
	if !intent.GraphEnabled || len(intent.Entities) < 2 {
		return false
	}
	switch intent.Route.EffectiveStrategy() {
	case domain.RecallStrategyJoin, domain.RecallStrategyIntersection:
		return true
	default:
		return len(intent.Entities) >= 2
	}
}

func kindsIntersectProfile(kinds []domain.FactKind) bool {
	if len(kinds) == 0 {
		return false
	}
	for _, k := range kinds {
		switch k {
		case domain.KindPreference, domain.KindRelation, domain.KindState, domain.KindParameter:
			return true
		}
	}
	return false
}

func (r *RecallStrategyPlanner) buildSourceOrder(intent domain.QueryIntent) []string {
	specs := r.Specs
	if len(specs) == 0 {
		specs = builtinSpecs()
	}
	var order []string
	for _, spec := range specs {
		if !strategyAllowsSource(intent, spec.Name) {
			continue
		}
		order = append(order, spec.Name)
	}
	return order
}

func (r *RecallStrategyPlanner) activeWeights(intent domain.QueryIntent, order []string) map[string]float64 {
	specs := r.Specs
	if len(specs) == 0 {
		specs = builtinSpecs()
	}
	byName := make(map[string]float64, len(specs))
	for _, spec := range specs {
		weight := spec.Weight
		if weight <= 0 {
			weight = 1
		}
		byName[spec.Name] = weight
	}
	out := make(map[string]float64, len(order))
	for _, source := range order {
		weight := byName[source]
		if weight <= 0 {
			weight = 1
		}
		out[source] = weight
	}
	return out
}

func strategyAllowsSource(intent domain.QueryIntent, source string) bool {
	switch source {
	case SourceRetrieval:
		return true
	case SourceTimeline:
		return ActivatesTimeline(intent)
	case SourceRelation:
		return ActivatesRelation(intent)
	case SourceAssertion:
		return ActivatesAssertion(intent)
	case SourceProfile:
		return ActivatesProfile(intent)
	case SourceGraph:
		return ActivatesGraph(intent)
	case SourceEntity:
		if len(intent.Entities) > 0 {
			return true
		}
		switch intent.Route.EffectiveStrategy() {
		case domain.RecallStrategySet, domain.RecallStrategyCount, domain.RecallStrategyIntersection, domain.RecallStrategyProfile:
			return true
		default:
			return false
		}
	default:
		return false
	}
}

// allocateBudgets splits limit across active sources. When only
// retrieval+entity are active the PR-3 RetrievalShare split applies.
// Otherwise budgets are weight-normalized to sum to limit. When
// limit < len(sources) the first limit sources each get budget 1.
func allocateBudgets(order []string, limit int, weights map[string]float64, retrievalShare float64) map[string]int {
	budgets := make(map[string]int, len(order))
	if len(order) == 0 {
		return budgets
	}
	total := sourceBudget(limit) * len(order)
	if retrievalShare > 0 && len(order) == 2 && order[0] == SourceRetrieval && order[1] == SourceEntity {
		retrievalBudget := int(math.Ceil(float64(total) * retrievalShare))
		budgets[SourceRetrieval] = clampSourceBudget(retrievalBudget)
		budgets[SourceEntity] = clampSourceBudget(total - retrievalBudget)
		return budgets
	}
	var weightSum float64
	for _, source := range order {
		weight := weights[source]
		if weight <= 0 {
			weight = 1
		}
		weightSum += weight
	}
	if weightSum <= 0 {
		weightSum = float64(len(order))
	}
	remaining := total
	for i, src := range order {
		weight := weights[src]
		if weight <= 0 {
			weight = 1
		}
		budget := int(math.Round(float64(total) * weight / weightSum))
		if i == len(order)-1 && remaining > 0 {
			budget = remaining
		}
		budget = clampSourceBudget(budget)
		budgets[src] = budget
		remaining -= budget
	}
	return budgets
}

// FusionCandidateCap computes the per-source fusion pool cap from the plan's
// final hit cap.
func FusionCandidateCap(finalCap int) int {
	if finalCap <= 0 {
		finalCap = DefaultLimit
	}
	cap := finalCap * FusionPoolMultiplier
	if cap > MaxFusionCandidateCap {
		cap = MaxFusionCandidateCap
	}
	if cap < finalCap {
		cap = finalCap
	}
	if cap < 1 {
		cap = 1
	}
	return cap
}

func clampSourceBudget(budget int) int {
	if budget < 1 {
		return 1
	}
	if budget > MaxSourceOverfetch {
		return MaxSourceOverfetch
	}
	return budget
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
