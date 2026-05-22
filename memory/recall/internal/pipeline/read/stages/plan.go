package stages

import (
	"context"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// EntitySnapshotsFunc returns per-sub-scope EntitySnapshot slices for
// the supplied scope list. The Plan stage merges the results across
// sub-scopes (Cluster G, D2 2026-05-21: plan is global; entity hints
// are pre-merged before the single planner.Plan call) and forwards
// the merged view to the planner as KnownEntities so federation_fanout
// no longer needs to re-invoke planner.Plan per sub-scope.
type EntitySnapshotsFunc func(scopes []domain.Scope) []port.EntitySnapshot

// Plan runs the planner ONCE per Recall, globally. Federation is a
// data dimension (per the 2026-05-21 D2 decision recorded in
// recall-v2-architecture-debts.md §7.3): the read pipeline uses a
// single QueryPlan for source budgeting, rank, and fuse — sub-scope
// fan-out happens downstream in federation_fanout, but the plan it
// consumes is this one.
type Plan struct {
	planner        port.Planner
	graphEnabled   bool
	entitySnapshot EntitySnapshotsFunc
}

// NewPlan constructs a Plan stage. entitySnapshot may be nil for
// tests that do not exercise the entity-hint path; when nil the
// stage skips the merge and forwards no KnownEntities to the planner.
func NewPlan(planner port.Planner, graphEnabled bool, entitySnapshot EntitySnapshotsFunc) *Plan {
	return &Plan{planner: planner, graphEnabled: graphEnabled, entitySnapshot: entitySnapshot}
}

// Name implements pipeline.Stage.
func (Plan) Name() string { return "plan" }

// Run implements pipeline.Stage.
func (s *Plan) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	if state.Intent == nil {
		return diagnostic.PlanDetail{}, nil
	}
	known := s.collectKnownEntities(state)
	plan, err := s.planner.Plan(ctx, port.PlannerInput{
		Scope:         state.Scope,
		Text:          state.Intent.Text,
		Entities:      state.Intent.Entities,
		Limit:         state.Intent.Limit,
		Subject:       state.Intent.Subject,
		Predicate:     state.Intent.Predicate,
		Object:        state.Intent.Object,
		Kinds:         state.Intent.Kinds,
		TimeRange:     state.Intent.TimeRange,
		GraphEnabled:  s.graphEnabled,
		GraphHops:     state.Query.GraphHops,
		KnownEntities: known,
	})
	if err != nil {
		return diagnostic.PlanDetail{}, err
	}
	state.Plan = &plan
	lenses := make([]diagnostic.ActivatedLens, 0, len(plan.SourceOrder))
	for _, name := range plan.SourceOrder {
		lenses = append(lenses, diagnostic.ActivatedLens{
			Lens:        name,
			Weight:      plan.LensWeights[name],
			Budget:      plan.SourceBudgets[name],
			ActivatedBy: "planner",
		})
	}
	return diagnostic.PlanDetail{
		ActivatedLenses: lenses,
		TotalBudget:     plan.TotalCap,
	}, nil
}

// collectKnownEntities pulls the per-sub-scope EntitySnapshot lists
// from the wired EntitySnapshotsFunc and folds them into a single
// merged slice. Single-scope reads still call this so the planner
// receives the same shape regardless of whether Federation is
// configured — federation is a data dimension, plan stays unified.
func (s *Plan) collectKnownEntities(state *read.ReadState) []port.EntitySnapshot {
	if s.entitySnapshot == nil {
		return nil
	}
	scopes := state.Scope.EffectiveFederation()
	if len(scopes) == 0 {
		scopes = []domain.Scope{state.Scope}
	}
	perScope := make([][]port.EntitySnapshot, 0, len(scopes))
	for _, sc := range scopes {
		snaps := s.entitySnapshot([]domain.Scope{sc})
		if len(snaps) == 0 {
			continue
		}
		perScope = append(perScope, snaps)
	}
	if len(perScope) == 0 {
		return nil
	}
	return mergeEntitySnapshots(perScope)
}

// mergeEntitySnapshots deduplicates per-sub-scope snapshots by
// case-insensitive canonical key. Weight reflects the "query focus"
// signal: each distinct sub-scope an entity appears in contributes
// at most 1 to the appearance count, then the merged Weight is the
// max of (a) that appearance count and (b) any pre-set Weight value
// the upstream snapshotter supplied (Cluster G, D2 2026-05-21).
// Aliases are unioned and sorted for stable output so the planner
// KnownEntities order is deterministic across runs. Empty /
// whitespace canonicals are dropped so the merge never emits a
// meaningless entry.
func mergeEntitySnapshots(perScope [][]port.EntitySnapshot) []port.EntitySnapshot {
	if len(perScope) == 0 {
		return nil
	}
	type entry struct {
		canonical    string
		appearances  int
		maxRawWeight float64
		aliases      map[string]struct{}
	}
	order := make([]string, 0)
	byKey := map[string]*entry{}
	for _, scopeSnaps := range perScope {
		seenInScope := map[string]bool{}
		for _, snap := range scopeSnaps {
			key := canonicalSnapshotKey(snap.Canonical)
			if key == "" {
				continue
			}
			e := byKey[key]
			if e == nil {
				e = &entry{canonical: snap.Canonical, aliases: map[string]struct{}{}}
				byKey[key] = e
				order = append(order, key)
			}
			if !seenInScope[key] {
				seenInScope[key] = true
				e.appearances++
			}
			if snap.Weight > e.maxRawWeight {
				e.maxRawWeight = snap.Weight
			}
			for _, alias := range snap.Aliases {
				if canonicalSnapshotKey(alias) == "" {
					continue
				}
				e.aliases[alias] = struct{}{}
			}
		}
	}
	out := make([]port.EntitySnapshot, 0, len(order))
	for _, key := range order {
		e := byKey[key]
		aliases := make([]string, 0, len(e.aliases))
		for a := range e.aliases {
			aliases = append(aliases, a)
		}
		sort.Strings(aliases)
		weight := float64(e.appearances)
		if e.maxRawWeight > weight {
			weight = e.maxRawWeight
		}
		out = append(out, port.EntitySnapshot{
			Canonical: e.canonical,
			Aliases:   aliases,
			Weight:    weight,
		})
	}
	return out
}

func canonicalSnapshotKey(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

var _ pipeline.Stage[*read.ReadState] = (*Plan)(nil)
