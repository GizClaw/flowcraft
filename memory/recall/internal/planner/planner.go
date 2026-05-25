// Package planner turns a caller Query into a deterministic
// QueryPlan. PR-3 ships a rule-based planner; the boundary keeps
// shape stable for an opt-in LLM intent parser in later phases
// (docs §9.1).
package planner

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
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
//
// The hierarchy reflects each source's role in the fused candidate
// set: retrieval is the lexical anchor and the only lane that
// reliably solo-wins, timeline is the rescue lane for temporal
// ("when did X happen") queries where BM25 ranks the date-bearing
// fact below the cap, and the remaining structured sources (graph,
// relation, profile, entity) contribute almost entirely as
// multi-source corroboration that raises the right fact under RRF
// when they agree. We keep retrieval at 1.0, give timeline the
// highest non-retrieval weight because of its rescue role, and pack
// the remaining structured sources in a narrow 0.85 band so they all
// contribute meaningfully to corroboration without any single one
// dominating.
const (
	WeightRetrieval = 1.0
	WeightTimeline  = 0.9
	WeightRelation  = 0.9
	WeightProfile   = 0.85
	WeightGraph     = 0.85
	WeightEntity    = 0.85
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

// RuleBased is the deterministic planner.
type RuleBased struct {
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

// New returns the default rule-based planner with built-in lens
// specs (tests and callers that do not use lens.Registry).
func New() *RuleBased { return NewFromSpecs(builtinSpecs()) }

// NewFromSpecs constructs a planner driven by the supplied lens
// registration order and activation predicates.
func NewFromSpecs(specs []LensSpec) *RuleBased {
	return &RuleBased{Specs: specs, RetrievalShare: 0.6}
}

// builtinSpecs mirrors the default lens.Registry registration order
// and activation rules used by memory.New.
func builtinSpecs() []LensSpec {
	return []LensSpec{
		{Name: SourceRetrieval, Weight: WeightRetrieval, Activate: func(domain.QueryIntent) bool { return true }},
		{Name: SourceEntity, Weight: WeightEntity, Activate: func(i domain.QueryIntent) bool { return len(i.Entities) > 0 }},
		{Name: SourceGraph, Weight: WeightGraph, Activate: ActivatesGraph},
		{Name: SourceRelation, Weight: WeightRelation, Activate: ActivatesRelation},
		{Name: SourceProfile, Weight: WeightProfile, Activate: ActivatesProfile},
		{Name: SourceTimeline, Weight: WeightTimeline, Activate: ActivatesTimeline},
	}
}

var _ port.Planner = (*RuleBased)(nil)

// Plan returns the QueryPlan with normalized limits and per-source
// budgets.
func (r *RuleBased) Plan(_ context.Context, input port.PlannerInput) (domain.QueryPlan, error) {
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
		Scope:        input.Scope,
		Limit:        limit,
		GraphEnabled: input.GraphEnabled && r.GraphEnabled,
		GraphHops:    input.GraphHops,
	}

	order := r.buildSourceOrder(intent)
	budgets := allocateBudgets(order, limit)
	weights := knownEntityLensWeights(order, intent, input.KnownEntities)

	return domain.QueryPlan{
		Intent:        intent,
		SourceOrder:   order,
		SourceBudgets: budgets,
		TotalCap:      limit,
		LensWeights:   weights,
	}, nil
}

// EntityHintBoost is the additive lens-weight bump applied per matching
// canonical / alias surface (Cluster G, D2 2026-05-21). Kept small and
// deterministic: the goal is to make the entity-hint signal observable
// downstream, not to overtake activation rules. The hint is also scaled
// by EntitySnapshot.Weight (the merge helper sets that to the number
// of sub-scopes the entity appeared in) so federation-wide focus
// entities outweigh single-scope mentions.
const EntityHintBoost = 0.05

// entityHintLenses is the static set of lenses that benefit from
// "query focus entity" hints. Retrieval and timeline are intentionally
// excluded — retrieval is the lexical anchor and always at weight 1.0;
// timeline activation is driven by TimeRange / Kinds rather than
// entity overlap.
var entityHintLenses = map[string]bool{
	SourceEntity:   true,
	SourceRelation: true,
	SourceGraph:    true,
	SourceProfile:  true,
}

// knownEntityLensWeights derives the optional lens-weight boost map
// from the planner's KnownEntities input. Returns nil when no hint
// could be applied so downstream consumers can cheaply detect "no
// boost" without map lookups.
func knownEntityLensWeights(order []string, intent domain.QueryIntent, known []port.EntitySnapshot) map[string]float64 {
	if len(order) == 0 || len(known) == 0 {
		return nil
	}
	terms := collectQueryTerms(intent)
	if len(terms) == 0 {
		return nil
	}
	var totalMatch float64
	for _, snap := range known {
		w := snap.Weight
		if w <= 0 {
			w = 1
		}
		if intersectsTerms(snap.Canonical, terms) {
			totalMatch += w
			continue
		}
		for _, alias := range snap.Aliases {
			if intersectsTerms(alias, terms) {
				totalMatch += w
				break
			}
		}
	}
	if totalMatch == 0 {
		return nil
	}
	weights := make(map[string]float64, len(order))
	boost := EntityHintBoost * totalMatch
	for _, name := range order {
		if entityHintLenses[name] {
			weights[name] = boost
		}
	}
	if len(weights) == 0 {
		return nil
	}
	return weights
}

func collectQueryTerms(intent domain.QueryIntent) map[string]struct{} {
	terms := map[string]struct{}{}
	add := func(s string) {
		k := canonicalEntityKey(s)
		if k != "" {
			terms[k] = struct{}{}
		}
	}
	add(intent.Subject)
	add(intent.Object)
	for _, e := range intent.Entities {
		add(e)
	}
	if intent.Text != "" {
		for _, tok := range strings.Fields(intent.Text) {
			add(tok)
		}
	}
	return terms
}

func intersectsTerms(s string, terms map[string]struct{}) bool {
	k := canonicalEntityKey(s)
	if k == "" {
		return false
	}
	if _, ok := terms[k]; ok {
		return true
	}
	for _, tok := range strings.Fields(k) {
		if _, ok := terms[tok]; ok {
			return true
		}
	}
	return false
}

// canonicalEntityKey trims surrounding whitespace and lowercases the
// input. The ingest pipeline owns the authoritative canonical-form
// helper (internal/ingest/normalizer.go: canonicalSpace) but it is
// unexported; this is a deliberately conservative duplicate that
// matches the case-insensitive trim semantics the planner needs for
// known-entity hint comparison.
func canonicalEntityKey(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ActivatesTimeline reports whether the timeline source should run.
func ActivatesTimeline(intent domain.QueryIntent) bool {
	if !intent.TimeRange.IsZero() {
		return true
	}
	if DirectTimelineDateIntent(intent.Features) {
		return true
	}
	if intent.Features.Temporal.HasIntent {
		return false
	}
	return kindsIntersectTimeline(intent.Kinds)
}

// ActivatesRelation reports whether the relation source should run.
func ActivatesRelation(intent domain.QueryIntent) bool {
	return intent.Subject != "" || intent.Predicate != "" || intent.Object != ""
}

// ActivatesProfile reports whether the profile source should run.
func ActivatesProfile(intent domain.QueryIntent) bool {
	return intent.Subject != ""
}

// ActivatesGraph reports whether bounded graph expansion should run.
func ActivatesGraph(intent domain.QueryIntent) bool {
	return intent.GraphEnabled && len(intent.Entities) > 0
}

func (r *RuleBased) buildSourceOrder(intent domain.QueryIntent) []string {
	specs := r.Specs
	if len(specs) == 0 {
		specs = builtinSpecs()
	}
	var order []string
	for _, spec := range specs {
		if spec.Activate != nil && !spec.Activate(intent) {
			continue
		}
		order = append(order, spec.Name)
	}
	return order
}

func kindsIntersectTimeline(kinds []domain.FactKind) bool {
	if len(kinds) == 0 {
		return false
	}
	for _, k := range kinds {
		switch k {
		case domain.KindEvent, domain.KindPlan, domain.KindState:
			return true
		}
	}
	return false
}

// DirectTimelineDateIntent reports whether query understanding indicates a
// direct "when/date" ask rather than a broad range/order/duration cue.
func DirectTimelineDateIntent(features domain.QueryFeatures) bool {
	temporal := features.Temporal
	if temporal.HasExplicitDate || !temporal.TimeRange.IsZero() {
		return true
	}
	if !hasTemporalIntentKind(temporal.IntentKind, domain.QueryTemporalIntentDate) {
		return false
	}
	return !temporal.HasDurationIntent &&
		!hasTemporalIntentKind(temporal.IntentKind, domain.QueryTemporalIntentDuration) &&
		!hasTemporalIntentKind(temporal.IntentKind, domain.QueryTemporalIntentRange) &&
		!hasTemporalIntentKind(temporal.IntentKind, domain.QueryTemporalIntentOrder)
}

func hasTemporalIntentKind(kinds []domain.QueryTemporalIntentKind, want domain.QueryTemporalIntentKind) bool {
	for _, kind := range kinds {
		if kind == want {
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

// FusionCandidateCap computes the per-source fusion pool cap from the
// plan's final hit cap (wired by memory.New into federation_fanout).
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
