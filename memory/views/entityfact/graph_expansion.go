package entityfact

import (
	"context"
	"math"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/memory/views"
)

const (
	GraphOriginFact   = "graph_fact"
	GraphOriginBridge = "graph_bridge"
)

type GraphExpansionOptions struct {
	MaxCandidates             int
	MaxFactsPerSeed           int
	MaxBridgeFacts            int
	MaxSourceRefsPerGraphPath int
}

type GraphSeedFact struct {
	Fact  Fact
	Score float64
}

type GraphSourceCandidate struct {
	SourceRef     views.SourceRef
	Origin        string
	FactIDs       []FactID
	SeedEntityIDs []EntityID
	Paths         []string
	Score         float64
}

type GraphExpansionResult struct {
	Seeds      []GraphSeedFact
	Candidates []GraphSourceCandidate
}

func ExpandGraphSources(ctx context.Context, store Store, scope views.Scope, seedFacts []GraphSeedFact, opts GraphExpansionOptions) (GraphExpansionResult, error) {
	if store == nil || len(seedFacts) == 0 {
		return GraphExpansionResult{}, nil
	}
	opts = normalizeGraphExpansionOptions(opts)
	seeds := normalizeGraphSeedFacts(seedFacts)
	if len(seeds) == 0 {
		return GraphExpansionResult{}, nil
	}
	seedEntities := map[EntityID]bool{}
	seedFactIDs := map[FactID]bool{}
	for _, seed := range seeds {
		seedFactIDs[seed.Fact.ID] = true
		for _, id := range factEntityIDs(seed.Fact) {
			seedEntities[id] = true
		}
	}

	bySource := map[string]GraphSourceCandidate{}
	oneHop := map[FactID]rankedFact{}
	for _, seed := range seeds {
		seedScore := seed.Score * graphFactConfidence(seed.Fact)
		seedIDs := factEntityIDs(seed.Fact)
		addFactSourceCandidates(bySource, seed.Fact, seedIDs, GraphOriginFact, seedScore, opts)
		oneHop[seed.Fact.ID] = rankedFact{fact: seed.Fact, score: seedScore}

		ranked, err := adjacentFactsForSeed(ctx, store, scope, seed, seedFactIDs)
		if err != nil {
			return GraphExpansionResult{}, err
		}
		for _, rankedFact := range ranked[:min(len(ranked), opts.MaxFactsPerSeed)] {
			if existing, ok := oneHop[rankedFact.fact.ID]; !ok || rankedFact.score > existing.score {
				oneHop[rankedFact.fact.ID] = rankedFact
			}
			addFactSourceCandidates(bySource, rankedFact.fact, seedIDs, GraphOriginFact, rankedFact.score, opts)
		}
	}

	bridgeAdded := 0
	for _, firstHop := range sortedRankedFacts(oneHop) {
		if bridgeAdded >= opts.MaxBridgeFacts {
			break
		}
		bridges, err := bridgeFacts(ctx, store, scope, firstHop.fact)
		if err != nil {
			return GraphExpansionResult{}, err
		}
		ranked := rankFacts(firstHop.score*0.45, bridges)
		for _, bridge := range ranked {
			if bridgeAdded >= opts.MaxBridgeFacts {
				break
			}
			if bridge.fact.ID == firstHop.fact.ID || seedFactIDs[bridge.fact.ID] {
				continue
			}
			seedIDs := sharedSeedEntities(firstHop.fact, seedEntities)
			addBridgeSourceCandidates(bySource, firstHop.fact, bridge.fact, seedIDs, bridge.score, opts)
			bridgeAdded++
		}
	}

	candidates := make([]GraphSourceCandidate, 0, len(bySource))
	for _, candidate := range bySource {
		candidates = append(candidates, candidate)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score == candidates[j].Score {
			return sourceRefSortKey(candidates[i].SourceRef) < sourceRefSortKey(candidates[j].SourceRef)
		}
		return candidates[i].Score > candidates[j].Score
	})
	if len(candidates) > opts.MaxCandidates {
		candidates = candidates[:opts.MaxCandidates]
	}
	return GraphExpansionResult{Seeds: seeds, Candidates: candidates}, nil
}

type rankedFact struct {
	fact  Fact
	score float64
}

func normalizeGraphExpansionOptions(opts GraphExpansionOptions) GraphExpansionOptions {
	if opts.MaxCandidates <= 0 {
		opts.MaxCandidates = 8
	}
	if opts.MaxFactsPerSeed <= 0 {
		opts.MaxFactsPerSeed = 8
	}
	if opts.MaxBridgeFacts <= 0 {
		opts.MaxBridgeFacts = 8
	}
	if opts.MaxSourceRefsPerGraphPath <= 0 {
		opts.MaxSourceRefsPerGraphPath = 2
	}
	return opts
}

func normalizeGraphSeedFacts(in []GraphSeedFact) []GraphSeedFact {
	byID := map[FactID]GraphSeedFact{}
	for _, seed := range in {
		if seed.Fact.ID == "" || !IsGraphableFact(seed.Fact) {
			continue
		}
		seed.Fact = CloneFact(seed.Fact)
		if seed.Score <= 0 {
			seed.Score = graphFactConfidence(seed.Fact)
		}
		if existing, ok := byID[seed.Fact.ID]; ok && existing.Score >= seed.Score {
			continue
		}
		byID[seed.Fact.ID] = seed
	}
	out := make([]GraphSeedFact, 0, len(byID))
	for _, seed := range byID {
		out = append(out, seed)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].Fact.ID < out[j].Fact.ID
		}
		return out[i].Score > out[j].Score
	})
	return out
}

func adjacentFactsForSeed(ctx context.Context, store Store, scope views.Scope, seed GraphSeedFact, seedFactIDs map[FactID]bool) ([]rankedFact, error) {
	byID := map[FactID]rankedFact{}
	for _, id := range factEntityIDs(seed.Fact) {
		facts, err := store.ListFactsByEntity(ctx, scope, id, ListOptions{Limit: 24})
		if err != nil {
			return nil, err
		}
		ranked := rankFacts(seed.Score, facts)
		for _, next := range ranked {
			if next.fact.ID == seed.Fact.ID || seedFactIDs[next.fact.ID] {
				continue
			}
			if existing, ok := byID[next.fact.ID]; !ok || next.score > existing.score {
				byID[next.fact.ID] = next
			}
		}
	}
	return sortedRankedFacts(byID), nil
}

func rankFacts(baseScore float64, facts []Fact) []rankedFact {
	degreePenalty := hubPenalty(len(facts))
	ranked := make([]rankedFact, 0, len(facts))
	for _, fact := range facts {
		if !IsGraphableFact(fact) {
			continue
		}
		score := baseScore * graphFactConfidence(fact) * degreePenalty
		ranked = append(ranked, rankedFact{fact: CloneFact(fact), score: score})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score == ranked[j].score {
			return ranked[i].fact.ID < ranked[j].fact.ID
		}
		return ranked[i].score > ranked[j].score
	})
	return ranked
}

func graphFactConfidence(fact Fact) float64 {
	confidence := fact.Confidence
	if confidence <= 0 {
		return 0.8
	}
	if confidence > 1 {
		return 1
	}
	return confidence
}

func sortedRankedFacts(facts map[FactID]rankedFact) []rankedFact {
	out := make([]rankedFact, 0, len(facts))
	for _, fact := range facts {
		out = append(out, fact)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score == out[j].score {
			return out[i].fact.ID < out[j].fact.ID
		}
		return out[i].score > out[j].score
	})
	return out
}

func bridgeFacts(ctx context.Context, store Store, scope views.Scope, fact Fact) ([]Fact, error) {
	byID := map[FactID]Fact{}
	addFacts := func(facts []Fact) {
		for _, next := range facts {
			byID[next.ID] = next
		}
	}
	for _, id := range factEntityIDs(fact) {
		facts, err := store.ListFactsByEntity(ctx, scope, id, ListOptions{Limit: 24})
		if err != nil {
			return nil, err
		}
		addFacts(facts)
	}
	if timeKey := NormalizeTimeKey(fact.TimeText); timeKey != "" {
		facts, err := store.ListFactsByTime(ctx, scope, timeKey, ListOptions{Limit: 24})
		if err != nil {
			return nil, err
		}
		addFacts(facts)
	}
	out := make([]Fact, 0, len(byID))
	for _, next := range byID {
		out = append(out, next)
	}
	return out, nil
}

func addFactSourceCandidates(bySource map[string]GraphSourceCandidate, fact Fact, seedIDs []EntityID, origin string, score float64, opts GraphExpansionOptions) {
	path := graphFactPath(seedIDs, fact.ID)
	addSourceCandidates(bySource, fact.SourceRefs, origin, []FactID{fact.ID}, seedIDs, []string{path}, score, opts.MaxSourceRefsPerGraphPath)
}

func addBridgeSourceCandidates(bySource map[string]GraphSourceCandidate, first, second Fact, seedIDs []EntityID, score float64, opts GraphExpansionOptions) {
	path := graphBridgePath(seedIDs, first.ID, second.ID)
	addSourceCandidates(bySource, second.SourceRefs, GraphOriginBridge, []FactID{first.ID, second.ID}, seedIDs, []string{path}, score, opts.MaxSourceRefsPerGraphPath)
}

func addSourceCandidates(bySource map[string]GraphSourceCandidate, refs []views.SourceRef, origin string, factIDs []FactID, seedIDs []EntityID, paths []string, score float64, maxRefs int) {
	perPath := 0
	for _, ref := range refs {
		if perPath >= maxRefs {
			break
		}
		if ref.Kind != views.SourceMessage || ref.Message == nil {
			continue
		}
		key := sourceRefSortKey(ref)
		if key == "" {
			continue
		}
		candidate := GraphSourceCandidate{
			SourceRef:     cloneSourceRefs([]views.SourceRef{ref})[0],
			Origin:        origin,
			FactIDs:       append([]FactID(nil), factIDs...),
			SeedEntityIDs: append([]EntityID(nil), seedIDs...),
			Paths:         append([]string(nil), paths...),
			Score:         score / float64(perPath+1),
		}
		mergeGraphCandidate(bySource, key, candidate)
		perPath++
	}
}

func mergeGraphCandidate(bySource map[string]GraphSourceCandidate, key string, next GraphSourceCandidate) {
	existing, ok := bySource[key]
	if !ok {
		bySource[key] = next
		return
	}
	if existing.Origin != GraphOriginBridge && next.Origin == GraphOriginBridge {
		existing.Origin = GraphOriginBridge
	}
	existing.FactIDs = appendFactIDs(existing.FactIDs, next.FactIDs...)
	existing.SeedEntityIDs = appendEntityIDs(existing.SeedEntityIDs, next.SeedEntityIDs...)
	existing.Paths = appendLimitedStrings(existing.Paths, next.Paths, 4)
	existing.Score = math.Min(existing.Score+next.Score, 2.5)
	bySource[key] = existing
}

func hubPenalty(degree int) float64 {
	if degree <= 8 {
		return 1
	}
	return 1 / math.Sqrt(1+float64(degree-8)/8)
}

func factEntityIDs(fact Fact) []EntityID {
	ids := []EntityID{fact.SubjectEntityID}
	ids = append(ids, fact.ObjectEntityIDs...)
	return appendEntityIDs(nil, ids...)
}

func sharedSeedEntities(fact Fact, seedSet map[EntityID]bool) []EntityID {
	var out []EntityID
	for _, id := range factEntityIDs(fact) {
		if seedSet[id] {
			out = appendEntityIDs(out, id)
		}
	}
	return out
}

func graphFactPath(seedIDs []EntityID, factID FactID) string {
	return "entities:" + entityIDPathSegment(seedIDs) + " -> fact:" + string(factID)
}

func graphBridgePath(seedIDs []EntityID, firstID, secondID FactID) string {
	return "entities:" + entityIDPathSegment(seedIDs) + " -> fact:" + string(firstID) + " -> fact:" + string(secondID)
}

func entityIDPathSegment(ids []EntityID) string {
	if len(ids) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ids))
	for _, id := range appendEntityIDs(nil, ids...) {
		parts = append(parts, string(id))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func sourceRefSortKey(ref views.SourceRef) string {
	if ref.Message == nil {
		return ""
	}
	return ref.Message.ConversationID + ":" + ref.Message.MessageID
}

func appendFactIDs(in []FactID, ids ...FactID) []FactID {
	seen := map[FactID]bool{}
	out := make([]FactID, 0, len(in)+len(ids))
	for _, id := range in {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func appendEntityIDs(in []EntityID, ids ...EntityID) []EntityID {
	seen := map[EntityID]bool{}
	out := make([]EntityID, 0, len(in)+len(ids))
	for _, id := range in {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func appendLimitedStrings(in, values []string, limit int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in)+len(values))
	for _, value := range in {
		if limit > 0 && len(out) >= limit {
			break
		}
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	for _, value := range values {
		if limit > 0 && len(out) >= limit {
			break
		}
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
