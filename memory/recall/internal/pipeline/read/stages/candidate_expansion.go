package stages

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	recallintent "github.com/GizClaw/flowcraft/memory/recall/internal/intent"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

const (
	neighborCandidateSource         = "neighbor_candidate"
	neighborCandidateTextFloor      = 0.45
	neighborCandidateMaxScanDefault = 240
	neighborCandidatePerGroupCap    = 3
)

type CandidateExpansion struct {
	store port.TemporalStore
}

func NewCandidateExpansion(store port.TemporalStore) *CandidateExpansion {
	return &CandidateExpansion{store: store}
}

func (CandidateExpansion) Name() string { return "candidate_expansion" }

func (s *CandidateExpansion) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	read.PromoteMergedItems(state)
	if state == nil || state.Plan == nil {
		return diagnostic.CandidateExpansionDetail{}, nil
	}
	tasks := state.Plan.TaskIntents
	detail := diagnostic.CandidateExpansionDetail{
		InputCount:      len(state.MergedItems),
		OutputCount:     len(state.MergedItems),
		TaskIntents:     taskIntentStrings(tasks),
		SuggestedByTask: map[string]int{},
	}
	recordCandidateExpansionSuggestions(state, &detail)
	if s == nil || s.store == nil || !neighborCandidateEnabled(tasks) {
		finalizeCandidateExpansionDetail(state, &detail)
		return detail, nil
	}
	anchors := neighborCandidateAnchors(state.Plan.Intent)
	if len(anchors) == 0 && len(state.MergedItems) == 0 {
		finalizeCandidateExpansionDetail(state, &detail)
		return detail, nil
	}
	existing := map[string]struct{}{}
	for _, item := range state.MergedItems {
		if item.Fact.ID != "" {
			existing[item.Fact.ID] = struct{}{}
		}
	}
	maxAdds := neighborCandidateMaxAdds(state.Plan.TotalCap)
	scored := make([]neighborScoredItem, 0, maxAdds)
	for _, scope := range state.Scope.EffectiveFederation() {
		if err := ctx.Err(); err != nil {
			return detail, err
		}
		facts, err := neighborCandidateListFacts(ctx, s.store, scope, anchors, neighborCandidateScanLimit(state.Plan.TotalCap))
		if err != nil {
			if isContextError(err) {
				return detail, err
			}
			detail.Err = err.Error()
			continue
		}
		detail.Scanned += len(facts)
		for _, fact := range facts {
			if _, ok := existing[fact.ID]; ok || fact.ID == "" {
				continue
			}
			if !state.Query.IncludeRetired && domain.IsRetired(fact, state.Now) {
				continue
			}
			item := neighborCandidateItem(fact, state.Scope)
			score, ok := neighborCandidateScore(state.Plan.Intent.Features, item, state.MergedItems)
			if !ok {
				continue
			}
			scored = append(scored, neighborScoredItem{item: item, score: score})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].item.Fact.ID < scored[j].item.Fact.ID
	})
	groupCounts := map[string]int{}
	for _, candidate := range scored {
		if len(detail.AddedFactIDs) >= maxAdds {
			break
		}
		group := neighborCandidateGroup(candidate.item)
		if group != "" && groupCounts[group] >= neighborCandidatePerGroupCap {
			continue
		}
		state.MergedItems = append(state.MergedItems, candidate.item)
		existing[candidate.item.Fact.ID] = struct{}{}
		detail.AddedFactIDs = append(detail.AddedFactIDs, candidate.item.Fact.ID)
		if group != "" {
			groupCounts[group]++
		}
	}
	detail.Added = len(detail.AddedFactIDs)
	detail.OutputCount = len(state.MergedItems)
	finalizeCandidateExpansionDetail(state, &detail)
	return detail, nil
}

func recordCandidateExpansionSuggestions(state *read.ReadState, detail *diagnostic.CandidateExpansionDetail) {
	if state == nil || state.Plan == nil || detail == nil || !hasExpansionTask(state.Plan.TaskIntents) {
		return
	}
	seeds := candidateExpansionSuggestionSeeds(state.MergedItems, state.Plan.TotalCap)
	covered := candidateExpansionSuggestionCoveredTokens(state.Plan.Intent.Features, seeds)
	suggested := map[string]struct{}{}
	for i := range state.MergedItems {
		item := &state.MergedItems[i]
		if candidateExpansionSuggestionSeedContains(seeds, item.Fact.ID) {
			continue
		}
		textScore := candidateExpansionSuggestionTextScore(state.Plan.Intent.Features, *item)
		structuralSetSibling := hasTask(state.Plan.TaskIntents, domain.QueryTaskSetCompletion) && candidateExpansionSuggestionHasSetSibling(*item, seeds)
		if textScore < candidateExpansionSuggestionTextFloor && !structuralSetSibling {
			continue
		}
		if hasTask(state.Plan.TaskIntents, domain.QueryTaskSetCompletion) && candidateExpansionSuggestionSetCandidate(state.Plan.Intent.Features, *item, seeds, covered, structuralSetSibling) {
			recordCandidateExpansionSuggestion(detail, suggested, item.Fact.ID, domain.QueryTaskSetCompletion)
			continue
		}
		if hasTask(state.Plan.TaskIntents, domain.QueryTaskBridgeResolution) && candidateExpansionSuggestionBridgeCandidate(state.Plan.Intent.Features, *item, seeds, covered) {
			recordCandidateExpansionSuggestion(detail, suggested, item.Fact.ID, domain.QueryTaskBridgeResolution)
		}
	}
	detail.Suggested = len(suggested)
}

func recordCandidateExpansionSuggestion(detail *diagnostic.CandidateExpansionDetail, suggested map[string]struct{}, factID string, task domain.QueryTaskIntent) {
	if detail == nil || factID == "" {
		return
	}
	if _, ok := suggested[factID]; ok {
		return
	}
	suggested[factID] = struct{}{}
	detail.SuggestedFactIDs = append(detail.SuggestedFactIDs, factID)
	detail.SuggestedByTask[string(task)]++
}

func finalizeCandidateExpansionDetail(state *read.ReadState, detail *diagnostic.CandidateExpansionDetail) {
	if detail == nil {
		return
	}
	if len(detail.SuggestedByTask) == 0 {
		detail.SuggestedByTask = nil
	}
	if snapshotsEnabled(state) {
		detail.Items = candidateSnapshotPtr(contextItemSnapshots(state.MergedItems))
	}
}

type neighborScoredItem struct {
	item  domain.ContextItem
	score float64
}

func neighborCandidateEnabled(tasks []domain.QueryTaskIntent) bool {
	return hasTask(tasks, domain.QueryTaskSetCompletion) ||
		hasTask(tasks, domain.QueryTaskBridgeResolution) ||
		hasTask(tasks, domain.QueryTaskTemporalReasoning)
}

func neighborCandidateListFacts(ctx context.Context, store port.TemporalStore, scope domain.Scope, anchors []string, limit int) ([]domain.TemporalFact, error) {
	if len(anchors) == 0 {
		return nil, nil
	}
	seen := map[string]struct{}{}
	var out []domain.TemporalFact
	for _, anchor := range anchors {
		for _, entity := range entityLookupVariants(anchor) {
			facts, err := store.List(ctx, scope, port.ListQuery{Entities: []string{entity}, Limit: limit})
			if err != nil {
				return out, err
			}
			for _, fact := range facts {
				if _, ok := seen[fact.ID]; ok {
					continue
				}
				seen[fact.ID] = struct{}{}
				out = append(out, fact)
			}
		}
	}
	return out, nil
}

func neighborCandidateAnchors(intent domain.QueryIntent) []string {
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || containsStringFold(out, s) {
			return
		}
		out = append(out, s)
	}
	add(intent.Subject)
	add(intent.Object)
	for _, entity := range intent.Entities {
		add(entity)
	}
	for proper := range intent.Features.Proper {
		add(proper)
	}
	return out
}

func entityLookupVariants(anchor string) []string {
	anchor = strings.TrimSpace(anchor)
	if anchor == "" {
		return nil
	}
	lower := strings.ToLower(anchor)
	if lower == anchor {
		return []string{anchor}
	}
	return []string{anchor, lower}
}

func neighborCandidateScanLimit(totalCap int) int {
	limit := totalCap * 8
	if limit < neighborCandidateMaxScanDefault {
		limit = neighborCandidateMaxScanDefault
	}
	return limit
}

func neighborCandidateMaxAdds(totalCap int) int {
	if totalCap <= 0 {
		return 6
	}
	return min(12, max(4, totalCap/4))
}

func neighborCandidateItem(fact domain.TemporalFact, _ domain.Scope) domain.ContextItem {
	ids := make([]string, 0, len(fact.EvidenceRefs))
	for _, ref := range fact.EvidenceRefs {
		if ref.ID != "" {
			ids = append(ids, ref.ID)
			continue
		}
		if ref.MessageID != "" {
			ids = append(ids, ref.MessageID)
		}
	}
	return domain.ContextItem{
		Candidate: domain.Candidate{
			FactID:      fact.ID,
			Scope:       fact.Scope,
			Source:      neighborCandidateSource,
			Score:       0,
			EvidenceIDs: ids,
		},
		Fact:     fact,
		Evidence: append([]domain.EvidenceRef(nil), fact.EvidenceRefs...),
	}
}

func neighborCandidateScore(features domain.QueryFeatures, item domain.ContextItem, seeds []domain.ContextItem) (float64, bool) {
	textScore := candidateExpansionSuggestionTextScore(features, item)
	score := textScore
	for _, seed := range seeds {
		if sameSubjectPredicate(item.Fact, seed.Fact) {
			score += 1.0
			continue
		}
		if group := contextItemEvidenceGroup(item); group != "" && group == contextItemEvidenceGroup(seed) {
			score += 0.55
		}
	}
	if score > textScore {
		return score, true
	}
	if textScore >= neighborCandidateTextFloor && neighborCandidateTokenMatches(features, item) >= 2 {
		return score, true
	}
	// Temporal questions often need a dated neighbor whose wording only
	// weakly overlaps the query but carries the event anchor.
	if features.HasTimeSignal() && factHasTimeSignal(item.Fact) && textScore >= 0.12 {
		return score + 0.25, true
	}
	return 0, false
}

func neighborCandidateTokenMatches(features domain.QueryFeatures, item domain.ContextItem) int {
	if len(features.Tokens) == 0 {
		return 0
	}
	tokens := candidateExpansionSuggestionTokenSet(item)
	matched := 0
	for tok := range features.Tokens {
		if _, ok := tokens[tok]; ok {
			matched++
		}
	}
	return matched
}

func neighborCandidateGroup(item domain.ContextItem) string {
	if same := strings.ToLower(strings.TrimSpace(item.Fact.Subject + "\x00" + item.Fact.Predicate)); strings.TrimSpace(item.Fact.Subject) != "" && strings.TrimSpace(item.Fact.Predicate) != "" {
		return "sp:" + same
	}
	if group := contextItemEvidenceGroup(item); group != "" {
		return "eg:" + group
	}
	return ""
}

func factHasTimeSignal(fact domain.TemporalFact) bool {
	if fact.ValidFrom != nil || fact.ValidTo != nil || !fact.ObservedAt.IsZero() {
		return true
	}
	for _, ref := range fact.EvidenceRefs {
		if !ref.Timestamp.IsZero() {
			return true
		}
	}
	return recallintent.HasTimex(fact.Content+" "+fact.EvidenceText, timeNowUTC())
}

func containsStringFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

func timeNowUTC() time.Time {
	return time.Now().UTC()
}

var _ pipeline.Stage[*read.ReadState] = (*CandidateExpansion)(nil)
