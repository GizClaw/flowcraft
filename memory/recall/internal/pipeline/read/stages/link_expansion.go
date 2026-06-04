package stages

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

const (
	linkExpansionSource      = "link_expansion"
	linkExpansionScoreFactor = 0.82
	linkExpansionMinScore    = 0.05
	linkExpansionDefaultCap  = 8

	linkExpansionDefaultProcessedLinksCap = 64
	linkExpansionDefaultEvidenceRefsCap   = 8
	linkExpansionLinkEvidenceRefScanCap   = 8
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
	budget := newLinkExpansionBudget(state)
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
			addedIDs, scanned, err := s.expandObservationSupportedAssertions(ctx, state, item, item.Observation.ID, existing, maxAdds-detail.AddedFacts, observationLinkCache, budget)
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
		if !budget.hasLinkBudget() {
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
		links = budget.takeLinks(links)
		detail.ScannedLinks += len(links)
		for _, link := range links {
			if err := ctx.Err(); err != nil {
				return detail, err
			}
			other := otherNode(link, node)
			switch other.Kind {
			case domain.GraphNodeObservation:
				if budget.hasEvidenceBudget() {
					added := s.attachObservationEvidence(ctx, item, other.ID)
					budget.recordEvidenceRefs(added)
					detail.AddedEvidenceRefs += added
				}
				if detail.AddedFacts >= maxAdds {
					continue
				}
				addedIDs, scanned, err := s.expandObservationSupportedAssertions(ctx, state, item, other.ID, existing, maxAdds-detail.AddedFacts, observationLinkCache, budget)
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
				if budget.hasEvidenceBudget() {
					added := attachSpanEvidence(item, link, other.ID)
					budget.recordEvidenceRefs(added)
					detail.AddedEvidenceRefs += added
				}
				if detail.AddedFacts >= maxAdds {
					continue
				}
				addedIDs, scanned, err := s.expandObservationSpanSupportedAssertions(ctx, state, item, other.ID, existing, maxAdds-detail.AddedFacts, observationLinkCache, budget)
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
				addedIDs, scanned, err = s.expandSiblingSpanSupportedAssertions(ctx, state, item, link, other.ID, existing, maxAdds-detail.AddedFacts, observationLinkCache, budget)
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
				if detail.AddedFacts >= maxAdds || !linkCanExpandAssertionFromNode(link, node, other) {
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
					markExistingContextItemLinkExpansionProvenance(state, added.Fact.ID, link)
					continue
				}
				if !state.Query.IncludeRetired && domain.IsRetired(added.Fact, state.Now) {
					continue
				}
				markLinkExpansionProvenance(&added, link)
				state.MergedItems = append(state.MergedItems, added)
				existing[added.Fact.ID] = struct{}{}
				detail.AddedFacts++
				detail.AddedFactIDs = append(detail.AddedFactIDs, added.Fact.ID)
			}
		}
	}

	detail.OutputCount = len(state.MergedItems)
	detail.Latency = time.Since(started)
	if snapshotsEnabled(state) {
		detail.Items = candidateSnapshotPtr(contextItemSnapshots(state.MergedItems))
	}
	return detail, nil
}

type linkExpansionBudget struct {
	processedLinksRemaining    int
	addedEvidenceRefsRemaining int
}

func newLinkExpansionBudget(state *read.ReadState) *linkExpansionBudget {
	return &linkExpansionBudget{
		processedLinksRemaining:    linkExpansionMaxProcessedLinks(state),
		addedEvidenceRefsRemaining: linkExpansionMaxEvidenceRefs(state),
	}
}

func (b *linkExpansionBudget) hasLinkBudget() bool {
	return b == nil || b.processedLinksRemaining > 0
}

func (b *linkExpansionBudget) takeLinks(links []domain.FactLink) []domain.FactLink {
	if b == nil {
		return links
	}
	if b.processedLinksRemaining <= 0 {
		return nil
	}
	if len(links) > b.processedLinksRemaining {
		links = links[:b.processedLinksRemaining]
	}
	b.processedLinksRemaining -= len(links)
	return links
}

func (b *linkExpansionBudget) hasEvidenceBudget() bool {
	return b == nil || b.addedEvidenceRefsRemaining > 0
}

func (b *linkExpansionBudget) recordEvidenceRefs(n int) {
	if b == nil || n <= 0 {
		return
	}
	b.addedEvidenceRefsRemaining -= n
	if b.addedEvidenceRefsRemaining < 0 {
		b.addedEvidenceRefsRemaining = 0
	}
}

func (s *LinkExpansion) expandObservationSupportedAssertions(ctx context.Context, state *read.ReadState, seed *domain.ContextItem, observationID string, existing map[string]struct{}, maxAdds int, linkCache map[string][]domain.FactLink, budget *linkExpansionBudget) ([]string, int, error) {
	if s == nil || state == nil || seed == nil || observationID == "" || maxAdds <= 0 {
		return nil, 0, nil
	}
	if !budget.hasLinkBudget() {
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
	links = budget.takeLinks(links)
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
		if !state.Query.IncludeRetired && domain.IsRetired(added.Fact, state.Now) {
			continue
		}
		markLinkExpansionProvenance(&added, link)
		state.MergedItems = append(state.MergedItems, added)
		existing[added.Fact.ID] = struct{}{}
		addedIDs = append(addedIDs, added.Fact.ID)
	}
	return addedIDs, len(links), nil
}

func (s *LinkExpansion) expandObservationSpanSupportedAssertions(ctx context.Context, state *read.ReadState, seed *domain.ContextItem, spanID string, existing map[string]struct{}, maxAdds int, linkCache map[string][]domain.FactLink, budget *linkExpansionBudget) ([]string, int, error) {
	return s.expandEvidenceNodeSupportedAssertions(ctx, state, seed, domain.GraphNodeRef{Kind: domain.GraphNodeObservationSpan, ID: spanID}, existing, maxAdds, linkCache, budget)
}

func (s *LinkExpansion) expandSiblingSpanSupportedAssertions(ctx context.Context, state *read.ReadState, seed *domain.ContextItem, seedLink domain.FactLink, seedSpanID string, existing map[string]struct{}, maxAdds int, linkCache map[string][]domain.FactLink, budget *linkExpansionBudget) ([]string, int, error) {
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
			if !budget.hasLinkBudget() {
				break
			}
			if span.ID == "" || span.ID == seedSpanID {
				continue
			}
			node := domain.GraphNodeRef{Kind: domain.GraphNodeObservationSpan, ID: span.ID}
			siblingAdded, siblingScanned, err := s.expandEvidenceNodeSupportedAssertions(ctx, state, seed, node, existing, maxAdds-len(addedIDs), linkCache, budget)
			scanned += siblingScanned
			if err != nil {
				return addedIDs, scanned, err
			}
			addedIDs = append(addedIDs, siblingAdded...)
		}
	}
	return addedIDs, scanned, nil
}

func (s *LinkExpansion) expandEvidenceNodeSupportedAssertions(ctx context.Context, state *read.ReadState, seed *domain.ContextItem, node domain.GraphNodeRef, existing map[string]struct{}, maxAdds int, linkCache map[string][]domain.FactLink, budget *linkExpansionBudget) ([]string, int, error) {
	if s == nil || state == nil || seed == nil || node.ID == "" || maxAdds <= 0 {
		return nil, 0, nil
	}
	if !budget.hasLinkBudget() {
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
	links = budget.takeLinks(links)
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
		if !state.Query.IncludeRetired && domain.IsRetired(added.Fact, state.Now) {
			continue
		}
		markLinkExpansionProvenance(&added, link)
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
	for i, ref := range link.EvidenceRefs {
		if i >= linkExpansionLinkEvidenceRefScanCap {
			break
		}
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
	for i, ref := range link.EvidenceRefs {
		if i >= linkExpansionLinkEvidenceRefScanCap {
			break
		}
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

func markExistingContextItemLinkExpansionProvenance(state *read.ReadState, factID string, link domain.FactLink) {
	if state == nil || factID == "" {
		return
	}
	for i := range state.MergedItems {
		item := &state.MergedItems[i]
		if item.Fact.ID != factID && item.Candidate.ID != factID {
			continue
		}
		markLinkExpansionProvenance(item, link)
		return
	}
}

func markLinkExpansionProvenance(item *domain.ContextItem, link domain.FactLink) {
	if item == nil {
		return
	}
	item.Link = link
	if item.Candidate.Metadata == nil {
		item.Candidate.Metadata = map[string]any{}
	}
	item.Candidate.Metadata["sources"] = appendUniqueString(metadataSources(item.Candidate.Metadata), linkExpansionSource)
	signal := domain.DiscoverySignal{
		Source: linkExpansionSource,
		Kind:   "typed_link",
		Value:  string(link.Type),
		Score:  item.Candidate.Score,
	}
	domain.AddCandidateDiscoverySignal(&item.Candidate, signal)
	if item.Ref.Metadata == nil {
		item.Ref.Metadata = map[string]any{}
	}
	item.Ref.Metadata["sources"] = appendUniqueString(metadataSources(item.Ref.Metadata), linkExpansionSource)
	domain.AddCandidateDiscoverySignal(&item.Ref, signal)
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
	case domain.LinkSupports, domain.LinkDerivedFrom, domain.LinkAnswersSlot, domain.LinkResolvesTo, domain.LinkSameObservation, domain.LinkSameEventAs:
		return true
	default:
		return false
	}
}

func linkCanExpandAssertionFromNode(link domain.FactLink, node, other domain.GraphNodeRef) bool {
	if !linkCanExpandAssertion(link) {
		return false
	}
	switch link.Type {
	case domain.LinkSupports, domain.LinkDerivedFrom, domain.LinkAnswersSlot, domain.LinkResolvesTo:
		return link.From == node && link.To == other
	default:
		return true
	}
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

func linkExpansionMaxProcessedLinks(state *read.ReadState) int {
	if state != nil && state.Plan != nil && state.Plan.TotalCap > 0 {
		return min(linkExpansionDefaultProcessedLinksCap, max(8, state.Plan.TotalCap*4))
	}
	return linkExpansionDefaultProcessedLinksCap
}

func linkExpansionMaxEvidenceRefs(state *read.ReadState) int {
	if state != nil && state.Plan != nil && state.Plan.TotalCap > 0 {
		return min(linkExpansionDefaultEvidenceRefsCap, max(2, state.Plan.TotalCap))
	}
	return linkExpansionDefaultEvidenceRefsCap
}

var (
	_ pipeline.Stage[*read.ReadState]       = (*LinkExpansion)(nil)
	_ pipeline.Conditional[*read.ReadState] = (*LinkExpansion)(nil)
)
