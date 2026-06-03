package stages

import (
	"context"
	"errors"
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
	linkExpansionSource      = "link_expansion"
	linkExpansionScoreFactor = 0.82
	linkExpansionMinScore    = 0.05
	linkExpansionDefaultCap  = 8
)

// LinkExpansion expands the materialized candidate pool through the canonical
// Observation/Assertion/Link ledger. Observation links enrich evidence on the
// existing item; assertion-support links add a bounded neighbor candidate.
type LinkExpansion struct {
	temporal     port.TemporalStore
	observations port.ObservationStore
	links        port.LinkStore
}

func NewLinkExpansion(temporal port.TemporalStore, observations port.ObservationStore, links port.LinkStore) *LinkExpansion {
	return &LinkExpansion{temporal: temporal, observations: observations, links: links}
}

func (LinkExpansion) Name() string { return "link_expansion" }

func (s *LinkExpansion) Skip(_ context.Context, state *read.ReadState) (bool, diagnostic.StageDetail) {
	read.PromoteMergedItems(state)
	if s == nil || s.temporal == nil || s.observations == nil || s.links == nil {
		return true, diagnostic.LinkExpansionDetail{}
	}
	if state == nil || len(state.MergedItems) == 0 {
		return true, diagnostic.LinkExpansionDetail{}
	}
	return false, nil
}

func (s *LinkExpansion) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	started := time.Now()
	if state == nil || len(state.MergedItems) == 0 {
		return diagnostic.LinkExpansionDetail{}, nil
	}
	read.PromoteMergedItems(state)
	detail := diagnostic.LinkExpansionDetail{InputCount: len(state.MergedItems)}

	existing := make(map[string]struct{}, len(state.MergedItems))
	for _, item := range state.MergedItems {
		if item.Fact.ID != "" {
			existing[item.Fact.ID] = struct{}{}
		}
		if item.Candidate.ID != "" {
			existing[item.Candidate.ID] = struct{}{}
		}
	}
	maxAdds := linkExpansionMaxAdds(state)
	observationLinkCache := make(map[string][]domain.FactLink)

	for i := range state.MergedItems {
		if err := ctx.Err(); err != nil {
			return detail, err
		}
		item := &state.MergedItems[i]
		if item.Candidate.Kind == domain.GraphNodeObservation && item.Observation.ID != "" {
			if detail.AddedFacts >= maxAdds {
				continue
			}
			addedIDs, scanned, err := s.expandObservationSupportedAssertions(ctx, state, item, item.Observation.ID, existing, maxAdds-detail.AddedFacts, observationLinkCache)
			detail.ScannedLinks += scanned
			if err != nil {
				if isContextError(err) {
					return detail, err
				}
				detail.Err = err.Error()
				continue
			}
			detail.AddedFacts += len(addedIDs)
			detail.AddedFactIDs = append(detail.AddedFactIDs, addedIDs...)
			continue
		}
		node := domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: item.Fact.ID}
		if node.ID == "" {
			continue
		}
		links, err := s.links.FindByNode(ctx, item.Fact.Scope, node)
		if err != nil {
			if isContextError(err) {
				return detail, err
			}
			detail.Err = err.Error()
			continue
		}
		detail.ScannedLinks += len(links)
		for _, link := range links {
			if err := ctx.Err(); err != nil {
				return detail, err
			}
			other := otherNode(link, node)
			switch other.Kind {
			case domain.GraphNodeObservation:
				added := s.attachObservationEvidence(ctx, item, other.ID)
				detail.AddedEvidenceRefs += added
				if detail.AddedFacts >= maxAdds {
					continue
				}
				addedIDs, scanned, err := s.expandObservationSupportedAssertions(ctx, state, item, other.ID, existing, maxAdds-detail.AddedFacts, observationLinkCache)
				detail.ScannedLinks += scanned
				if err != nil {
					if isContextError(err) {
						return detail, err
					}
					detail.Err = err.Error()
					continue
				}
				detail.AddedFacts += len(addedIDs)
				detail.AddedFactIDs = append(detail.AddedFactIDs, addedIDs...)
			case domain.GraphNodeObservationSpan:
				added := attachSpanEvidence(item, link, other.ID)
				detail.AddedEvidenceRefs += added
				if detail.AddedFacts >= maxAdds {
					continue
				}
				addedIDs, scanned, err := s.expandObservationSpanSupportedAssertions(ctx, state, item, other.ID, existing, maxAdds-detail.AddedFacts, observationLinkCache)
				detail.ScannedLinks += scanned
				if err != nil {
					if isContextError(err) {
						return detail, err
					}
					detail.Err = err.Error()
					continue
				}
				detail.AddedFacts += len(addedIDs)
				detail.AddedFactIDs = append(detail.AddedFactIDs, addedIDs...)
				if detail.AddedFacts >= maxAdds {
					continue
				}
				addedIDs, scanned, err = s.expandSiblingSpanSupportedAssertions(ctx, state, item, link, other.ID, existing, maxAdds-detail.AddedFacts, observationLinkCache)
				detail.ScannedLinks += scanned
				if err != nil {
					if isContextError(err) {
						return detail, err
					}
					detail.Err = err.Error()
					continue
				}
				detail.AddedFacts += len(addedIDs)
				detail.AddedFactIDs = append(detail.AddedFactIDs, addedIDs...)
			case domain.GraphNodeAssertion:
				if detail.AddedFacts >= maxAdds || !linkCanExpandAssertion(link) {
					continue
				}
				added, ok, err := s.linkedAssertionItem(ctx, state, item, other.ID)
				if err != nil {
					if isContextError(err) {
						return detail, err
					}
					detail.Err = err.Error()
					continue
				}
				if !ok {
					continue
				}
				if _, exists := existing[added.Fact.ID]; exists {
					markContextItemSource(state, added.Fact.ID, linkExpansionSource)
					continue
				}
				if !state.Query.IncludeRetired && domain.IsRetired(added.Fact, state.Now) {
					continue
				}
				state.MergedItems = append(state.MergedItems, added)
				existing[added.Fact.ID] = struct{}{}
				detail.AddedFacts++
				detail.AddedFactIDs = append(detail.AddedFactIDs, added.Fact.ID)
			}
		}
		if detail.AddedFacts < maxAdds && linkExpansionBridgeTask(state) {
			bridgeStats, err := s.expandAdjacentEvidenceAssertions(ctx, state, item, existing, maxAdds-detail.AddedFacts, observationLinkCache)
			detail.ScannedLinks += bridgeStats.ScannedLinks
			recordAdjacentBridgeStats(&detail, bridgeStats)
			if err != nil {
				if isContextError(err) {
					return detail, err
				}
				detail.Err = err.Error()
			}
			detail.AddedFacts += len(bridgeStats.AddedFactIDs)
			detail.AddedFactIDs = append(detail.AddedFactIDs, bridgeStats.AddedFactIDs...)
		}
	}

	detail.OutputCount = len(state.MergedItems)
	detail.Latency = time.Since(started)
	if snapshotsEnabled(state) {
		detail.Items = candidateSnapshotPtr(contextItemSnapshots(state.MergedItems))
	}
	return detail, nil
}

type adjacentBridgeStats struct {
	Refs                  int
	ObservationScans      int
	MatchedObservationIDs []string
	ScannedLinks          int
	AddedFactIDs          []string
}

func recordAdjacentBridgeStats(detail *diagnostic.LinkExpansionDetail, stats adjacentBridgeStats) {
	if detail == nil {
		return
	}
	detail.AdjacentBridgeRefs += stats.Refs
	detail.AdjacentBridgeObservationScans += stats.ObservationScans
	detail.AdjacentBridgeMatchedObservations += len(stats.MatchedObservationIDs)
	detail.AdjacentBridgeMatchedObservationIDs = append(detail.AdjacentBridgeMatchedObservationIDs, stats.MatchedObservationIDs...)
	detail.AdjacentBridgeScannedLinks += stats.ScannedLinks
	detail.AdjacentBridgeAddedFacts += len(stats.AddedFactIDs)
	detail.AdjacentBridgeAddedFactIDs = append(detail.AdjacentBridgeAddedFactIDs, stats.AddedFactIDs...)
}

func (s *LinkExpansion) expandAdjacentEvidenceAssertions(ctx context.Context, state *read.ReadState, seed *domain.ContextItem, existing map[string]struct{}, maxAdds int, linkCache map[string][]domain.FactLink) (adjacentBridgeStats, error) {
	stats := adjacentBridgeStats{}
	if s == nil || state == nil || seed == nil || maxAdds <= 0 {
		return stats, nil
	}
	refs := seed.Evidence
	if len(refs) == 0 {
		refs = seed.Fact.EvidenceRefs
	}
	if len(refs) == 0 {
		return stats, nil
	}
	stats.Refs = len(refs)
	observations, err := s.observations.List(ctx, contextItemScope(*seed), port.ObservationListQuery{Limit: 1000})
	if err != nil {
		return stats, err
	}
	seenObservations := map[string]struct{}{}
	for _, ref := range refs {
		if len(stats.AddedFactIDs) >= maxAdds {
			break
		}
		for _, obs := range observations {
			stats.ObservationScans++
			if len(stats.AddedFactIDs) >= maxAdds {
				break
			}
			if obs.ID == "" || !observationAdjacentToEvidenceRef(obs, ref) {
				continue
			}
			if _, seen := seenObservations[obs.ID]; seen {
				continue
			}
			seenObservations[obs.ID] = struct{}{}
			stats.MatchedObservationIDs = append(stats.MatchedObservationIDs, obs.ID)
			for _, span := range obs.Spans {
				if len(stats.AddedFactIDs) >= maxAdds {
					break
				}
				if span.ID == "" {
					continue
				}
				node := domain.GraphNodeRef{Kind: domain.GraphNodeObservationSpan, ID: span.ID}
				spanAdded, spanScanned, err := s.expandEvidenceNodeSupportedAssertions(ctx, state, seed, node, existing, maxAdds-len(stats.AddedFactIDs), linkCache)
				stats.ScannedLinks += spanScanned
				if err != nil {
					return stats, err
				}
				stats.AddedFactIDs = append(stats.AddedFactIDs, spanAdded...)
			}
		}
	}
	return stats, nil
}

func linkExpansionBridgeTask(state *read.ReadState) bool {
	if state == nil || state.Plan == nil {
		return false
	}
	return hasTask(state.Plan.TaskIntents, domain.QueryTaskBridgeResolution) ||
		hasTask(state.Plan.TaskIntents, domain.QueryTaskSetCompletion)
}

func observationAdjacentToEvidenceRef(obs domain.Observation, ref domain.EvidenceRef) bool {
	if strings.TrimSpace(obs.SessionID) == "" || strings.TrimSpace(ref.SessionID) == "" || obs.SessionID != ref.SessionID {
		return false
	}
	obsSource := observationSourceID(obs)
	refSource := evidenceRefSourceID(ref)
	if obsSource == "" || refSource == "" || obsSource == refSource {
		return false
	}
	return sourceIDsAdjacent(obsSource, refSource)
}

func observationSourceID(obs domain.Observation) string {
	if id := strings.TrimSpace(obs.SourceID); id != "" {
		return id
	}
	return strings.TrimSpace(obs.MessageID)
}

func evidenceRefSourceID(ref domain.EvidenceRef) string {
	if id := strings.TrimSpace(ref.MessageID); id != "" {
		return id
	}
	return strings.TrimSpace(ref.ID)
}

func sourceIDsAdjacent(a, b string) bool {
	prefixA, nA, okA := splitSourceOrdinal(a)
	prefixB, nB, okB := splitSourceOrdinal(b)
	if !okA || !okB || prefixA != prefixB {
		return false
	}
	delta := nA - nB
	return delta == 1 || delta == -1
}

func splitSourceOrdinal(id string) (string, int, bool) {
	id = strings.TrimSpace(id)
	idx := strings.LastIndex(id, ":")
	if idx < 0 || idx == len(id)-1 {
		return "", 0, false
	}
	n := 0
	for _, r := range id[idx+1:] {
		if r < '0' || r > '9' {
			return "", 0, false
		}
		n = n*10 + int(r-'0')
	}
	return id[:idx], n, true
}

func (s *LinkExpansion) expandObservationSupportedAssertions(ctx context.Context, state *read.ReadState, seed *domain.ContextItem, observationID string, existing map[string]struct{}, maxAdds int, linkCache map[string][]domain.FactLink) ([]string, int, error) {
	if s == nil || state == nil || seed == nil || observationID == "" || maxAdds <= 0 {
		return nil, 0, nil
	}
	obsNode := domain.GraphNodeRef{Kind: domain.GraphNodeObservation, ID: observationID}
	links, ok := linkCache[observationID]
	if !ok {
		var err error
		links, err = s.links.FindByNode(ctx, contextItemScope(*seed), obsNode)
		if err != nil {
			return nil, 0, err
		}
		linkCache[observationID] = links
	}
	var addedIDs []string
	for _, link := range links {
		if len(addedIDs) >= maxAdds {
			break
		}
		if link.Type != domain.LinkSupports || link.From != obsNode || link.To.Kind != domain.GraphNodeAssertion {
			continue
		}
		if _, ok := existing[link.To.ID]; ok {
			continue
		}
		added, ok, err := s.linkedAssertionItem(ctx, state, seed, link.To.ID)
		if err != nil || !ok {
			return addedIDs, len(links), err
		}
		if !linkedAssertionMatchesQueryWithLink(state, added.Fact, link) {
			continue
		}
		if !state.Query.IncludeRetired && domain.IsRetired(added.Fact, state.Now) {
			continue
		}
		state.MergedItems = append(state.MergedItems, added)
		existing[added.Fact.ID] = struct{}{}
		addedIDs = append(addedIDs, added.Fact.ID)
	}
	return addedIDs, len(links), nil
}

func (s *LinkExpansion) expandObservationSpanSupportedAssertions(ctx context.Context, state *read.ReadState, seed *domain.ContextItem, spanID string, existing map[string]struct{}, maxAdds int, linkCache map[string][]domain.FactLink) ([]string, int, error) {
	return s.expandEvidenceNodeSupportedAssertions(ctx, state, seed, domain.GraphNodeRef{Kind: domain.GraphNodeObservationSpan, ID: spanID}, existing, maxAdds, linkCache)
}

func (s *LinkExpansion) expandSiblingSpanSupportedAssertions(ctx context.Context, state *read.ReadState, seed *domain.ContextItem, seedLink domain.FactLink, seedSpanID string, existing map[string]struct{}, maxAdds int, linkCache map[string][]domain.FactLink) ([]string, int, error) {
	if s == nil || state == nil || seed == nil || seedSpanID == "" || maxAdds <= 0 {
		return nil, 0, nil
	}
	var addedIDs []string
	var scanned int
	for _, observationID := range observationIDsForSpan(seedLink, seedSpanID) {
		if len(addedIDs) >= maxAdds {
			break
		}
		obs, err := s.observations.Get(ctx, contextItemScope(*seed), observationID)
		if err != nil {
			if errors.Is(err, port.ErrNotFound) {
				continue
			}
			return addedIDs, scanned, err
		}
		for _, span := range obs.Spans {
			if len(addedIDs) >= maxAdds {
				break
			}
			if span.ID == "" || span.ID == seedSpanID {
				continue
			}
			node := domain.GraphNodeRef{Kind: domain.GraphNodeObservationSpan, ID: span.ID}
			siblingAdded, siblingScanned, err := s.expandEvidenceNodeSupportedAssertions(ctx, state, seed, node, existing, maxAdds-len(addedIDs), linkCache)
			scanned += siblingScanned
			if err != nil {
				return addedIDs, scanned, err
			}
			addedIDs = append(addedIDs, siblingAdded...)
		}
	}
	return addedIDs, scanned, nil
}

func (s *LinkExpansion) expandEvidenceNodeSupportedAssertions(ctx context.Context, state *read.ReadState, seed *domain.ContextItem, node domain.GraphNodeRef, existing map[string]struct{}, maxAdds int, linkCache map[string][]domain.FactLink) ([]string, int, error) {
	if s == nil || state == nil || seed == nil || node.ID == "" || maxAdds <= 0 {
		return nil, 0, nil
	}
	cacheKey := string(node.Kind) + ":" + node.ID
	links, ok := linkCache[cacheKey]
	if !ok {
		var err error
		links, err = s.links.FindByNode(ctx, seed.Fact.Scope, node)
		if err != nil {
			return nil, 0, err
		}
		linkCache[cacheKey] = links
	}
	var addedIDs []string
	for _, link := range links {
		if len(addedIDs) >= maxAdds {
			break
		}
		if link.Type != domain.LinkSupports || link.From != node || link.To.Kind != domain.GraphNodeAssertion {
			continue
		}
		if _, ok := existing[link.To.ID]; ok {
			continue
		}
		added, ok, err := s.linkedAssertionItem(ctx, state, seed, link.To.ID)
		if err != nil || !ok {
			return addedIDs, len(links), err
		}
		if !linkedAssertionMatchesQueryWithLink(state, added.Fact, link) {
			continue
		}
		if !state.Query.IncludeRetired && domain.IsRetired(added.Fact, state.Now) {
			continue
		}
		state.MergedItems = append(state.MergedItems, added)
		existing[added.Fact.ID] = struct{}{}
		addedIDs = append(addedIDs, added.Fact.ID)
	}
	return addedIDs, len(links), nil
}

func observationIDsForSpan(link domain.FactLink, spanID string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, ref := range link.EvidenceRefs {
		if ref.SpanID != "" && ref.SpanID != spanID {
			continue
		}
		add(ref.ObservationID)
	}
	if len(out) == 0 {
		for _, id := range link.EvidenceObservationIDs {
			add(id)
		}
	}
	return out
}

func (s *LinkExpansion) attachObservationEvidence(ctx context.Context, item *domain.ContextItem, observationID string) int {
	if item == nil || observationID == "" {
		return 0
	}
	obs, err := s.observations.Get(ctx, item.Fact.Scope, observationID)
	if err != nil {
		return 0
	}
	ref := evidenceRefFromObservation(obs)
	if ref.Text == "" || evidenceDuplicatesItem(*item, ref) {
		return 0
	}
	item.Evidence = append(item.Evidence, ref)
	return 1
}

func attachSpanEvidence(item *domain.ContextItem, link domain.FactLink, spanID string) int {
	if item == nil || spanID == "" {
		return 0
	}
	for _, ref := range link.EvidenceRefs {
		if ref.SpanID != "" && ref.SpanID != spanID {
			continue
		}
		if ref.Text == "" || evidenceDuplicatesItem(*item, ref) {
			continue
		}
		item.Evidence = append(item.Evidence, ref)
		return 1
	}
	return 0
}

func (s *LinkExpansion) linkedAssertionItem(ctx context.Context, state *read.ReadState, seed *domain.ContextItem, factID string) (domain.ContextItem, bool, error) {
	if seed == nil || factID == "" {
		return domain.ContextItem{}, false, nil
	}
	fact, err := s.temporal.Get(ctx, contextItemScope(*seed), factID)
	if err != nil {
		if errors.Is(err, port.ErrNotFound) {
			return domain.ContextItem{}, false, nil
		}
		return domain.ContextItem{}, false, err
	}
	if fact.CorrectedBy != "" || (state != nil && !domain.ScopeVisible(state.Scope, fact.Scope)) {
		return domain.ContextItem{}, false, nil
	}
	score := seed.Candidate.Score * linkExpansionScoreFactor
	if score <= 0 {
		score = linkExpansionMinScore
	}
	return domain.ContextItem{
		Candidate: domain.Candidate{
			Kind:        domain.GraphNodeAssertion,
			ID:          fact.ID,
			Scope:       fact.Scope,
			Source:      linkExpansionSource,
			Score:       score,
			EvidenceIDs: evidenceIDsFromRefs(fact.EvidenceRefs),
			Metadata:    map[string]any{"sources": []string{linkExpansionSource}},
		},
		Ref:      domain.CandidateRef{Kind: domain.GraphNodeAssertion, ID: fact.ID, Scope: fact.Scope, Source: linkExpansionSource, Score: score},
		Fact:     fact,
		Evidence: append([]domain.EvidenceRef(nil), fact.EvidenceRefs...),
	}, true, nil
}

func markContextItemSource(state *read.ReadState, factID, source string) {
	if state == nil || factID == "" || source == "" {
		return
	}
	for i := range state.MergedItems {
		item := &state.MergedItems[i]
		if item.Fact.ID != factID && item.Candidate.ID != factID {
			continue
		}
		if item.Candidate.Metadata == nil {
			item.Candidate.Metadata = map[string]any{}
		}
		item.Candidate.Metadata["sources"] = appendUniqueString(metadataSources(item.Candidate.Metadata), source)
		if item.Ref.Metadata == nil {
			item.Ref.Metadata = map[string]any{}
		}
		item.Ref.Metadata["sources"] = appendUniqueString(metadataSources(item.Ref.Metadata), source)
		return
	}
}

func metadataSources(md map[string]any) []string {
	if len(md) == 0 {
		return nil
	}
	switch v := md["sources"].(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func appendUniqueString(in []string, value string) []string {
	for _, existing := range in {
		if existing == value {
			return in
		}
	}
	return append(in, value)
}

func contextItemScope(item domain.ContextItem) domain.Scope {
	if item.Fact.Scope.RuntimeID != "" {
		return item.Fact.Scope
	}
	if item.Observation.Scope.RuntimeID != "" {
		return item.Observation.Scope
	}
	if item.Link.Scope.RuntimeID != "" {
		return item.Link.Scope
	}
	return item.Candidate.Scope
}

func otherNode(link domain.FactLink, node domain.GraphNodeRef) domain.GraphNodeRef {
	if link.From == node {
		return link.To
	}
	if link.To == node {
		return link.From
	}
	return domain.GraphNodeRef{}
}

func linkCanExpandAssertion(link domain.FactLink) bool {
	switch link.Type {
	case domain.LinkSupports, domain.LinkSameObservation, domain.LinkSameEventAs:
		return true
	default:
		return false
	}
}

func linkedAssertionMatchesQueryWithLink(state *read.ReadState, fact domain.TemporalFact, link domain.FactLink) bool {
	if link.Type == domain.LinkSupports && link.From.Kind == domain.GraphNodeAssertion && link.To.Kind == domain.GraphNodeAssertion {
		return true
	}
	return linkedAssertionMatchesQueryText(state, strings.Join([]string{
		fact.Subject,
		fact.Content,
		fact.EvidenceText,
		evidenceTextForMatch(fact.EvidenceRefs),
		evidenceTextForMatch(link.EvidenceRefs),
	}, " "))
}

func linkedAssertionMatchesQueryText(state *read.ReadState, text string) bool {
	queryTokens := linkExpansionQueryTokens(state)
	if len(queryTokens) == 0 {
		return false
	}
	textTokens := recallintent.TextTokenSet(text)
	if linkExpansionMatchesExplicitAnchor(state, textTokens) {
		return true
	}
	overlap := 0
	for token := range queryTokens {
		if _, ok := textTokens[token]; ok {
			overlap++
		}
	}
	if len(queryTokens) <= 2 {
		return overlap == len(queryTokens)
	}
	return overlap >= 3 && float64(overlap)/float64(len(queryTokens)) >= 0.55
}

func linkExpansionMatchesExplicitAnchor(state *read.ReadState, textTokens map[string]struct{}) bool {
	features := linkExpansionFeatures(state)
	return tokenSetIntersects(features.Proper, textTokens) ||
		tokenSetIntersects(features.Numeric, textTokens) ||
		tokenSetIntersects(features.Quoted, textTokens) ||
		tokenSetIntersects(linkExpansionEntityTokens(state), textTokens)
}

func linkExpansionFeatures(state *read.ReadState) domain.QueryFeatures {
	if state != nil && state.Plan != nil {
		return state.Plan.Intent.Features
	}
	if state != nil && state.Intent != nil {
		return state.Intent.Features
	}
	return domain.QueryFeatures{}
}

func linkExpansionEntityTokens(state *read.ReadState) map[string]struct{} {
	var values []string
	if state != nil && state.Plan != nil {
		values = append(values, state.Plan.Intent.Entities...)
		values = append(values, state.Plan.Intent.Subject, state.Plan.Intent.Object)
	} else if state != nil && state.Intent != nil {
		values = append(values, state.Intent.Entities...)
		values = append(values, state.Intent.Subject, state.Intent.Object)
	}
	if len(values) == 0 {
		return nil
	}
	out := map[string]struct{}{}
	for _, value := range values {
		for token := range recallintent.TextTokenSet(value) {
			out[token] = struct{}{}
		}
	}
	return out
}

func linkExpansionQueryTokens(state *read.ReadState) map[string]struct{} {
	if state != nil && state.Plan != nil && len(state.Plan.Intent.Features.Tokens) > 0 {
		return state.Plan.Intent.Features.Tokens
	}
	if state != nil && state.Intent != nil && len(state.Intent.Features.Tokens) > 0 {
		return state.Intent.Features.Tokens
	}
	if state == nil {
		return nil
	}
	return recallintent.TextTokenSet(state.Query.Text)
}

func evidenceTextForMatch(refs []domain.EvidenceRef) string {
	if len(refs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.Text != "" {
			parts = append(parts, ref.Text)
		}
	}
	return strings.Join(parts, " ")
}

func evidenceRefFromObservation(obs domain.Observation) domain.EvidenceRef {
	ts := obs.ObservedAt
	if ts.IsZero() {
		ts = obs.ReceivedAt
	}
	return domain.EvidenceRef{
		ID:            obs.ID,
		ObservationID: obs.ID,
		MessageID:     obs.MessageID,
		Role:          obs.Role,
		Text:          obs.Text,
		Timestamp:     ts,
	}
}

func evidenceRefExists(refs []domain.EvidenceRef, want domain.EvidenceRef) bool {
	wantKey := evidenceRefKey(want)
	wantText := comparableEvidenceText(want.Text)
	for _, ref := range refs {
		if evidenceRefKey(ref) == wantKey {
			return true
		}
		if wantText != "" && comparableEvidenceText(ref.Text) == wantText {
			return true
		}
	}
	return false
}

func evidenceDuplicatesItem(item domain.ContextItem, ref domain.EvidenceRef) bool {
	text := comparableEvidenceText(ref.Text)
	if text == "" {
		return true
	}
	if comparableEvidenceText(item.Fact.Content) == text || comparableEvidenceText(item.Fact.EvidenceText) == text {
		return true
	}
	return evidenceRefExists(item.Evidence, ref) || evidenceRefExists(item.Fact.EvidenceRefs, ref)
}

func comparableEvidenceText(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

func evidenceIDsFromRefs(refs []domain.EvidenceRef) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.SpanID != "" {
			out = append(out, ref.SpanID)
			continue
		}
		if ref.ObservationID != "" {
			out = append(out, ref.ObservationID)
			continue
		}
		if ref.ID != "" {
			out = append(out, ref.ID)
			continue
		}
		if ref.MessageID != "" {
			out = append(out, ref.MessageID)
		}
	}
	return out
}

func linkExpansionMaxAdds(state *read.ReadState) int {
	if state != nil && state.Plan != nil && state.Plan.TotalCap > 0 {
		return min(linkExpansionDefaultCap, max(2, state.Plan.TotalCap/2))
	}
	return linkExpansionDefaultCap
}

var (
	_ pipeline.Stage[*read.ReadState]       = (*LinkExpansion)(nil)
	_ pipeline.Conditional[*read.ReadState] = (*LinkExpansion)(nil)
)
