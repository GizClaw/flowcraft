package context

import (
	stdctx "context"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewentityfact "github.com/GizClaw/flowcraft/memory/views/entityfact"
)

type SourceEvidenceOrigin string

const (
	SourceEvidenceOriginDirect       SourceEvidenceOrigin = "direct"
	SourceEvidenceOriginSummary      SourceEvidenceOrigin = "summary"
	SourceEvidenceOriginEntityFact   SourceEvidenceOrigin = "entity_fact"
	SourceEvidenceOriginGraph        SourceEvidenceOrigin = "graph"
	SourceEvidenceOriginNeighborhood SourceEvidenceOrigin = "source_neighborhood"
)

const (
	SourceEvidenceOriginMetadataKey         = "source_evidence_origin"
	SourceEvidenceOriginsMetadataKey        = "source_evidence_origins"
	SourceEvidenceScoreMetadataKey          = "source_evidence_score"
	SourceEvidenceSummaryNodeIDsMetadataKey = "summary_node_ids"
	SourceEvidenceEntityFactIDsMetadataKey  = "entity_fact_ids"

	GraphOriginMetadataKey  = "graph_origin"
	GraphFactIDsMetadataKey = "graph_fact_ids"
	GraphPathMetadataKey    = "graph_path"
	GraphScoreMetadataKey   = "graph_score"
	GraphSeedIDsMetadataKey = "graph_seed_entity_ids"
)

type SourceEvidenceOriginValues struct {
	Direct       string
	Summary      string
	EntityFact   string
	Graph        string
	Neighborhood string
}

// SourceEvidencePacker selects canonical source-message evidence referenced by
// retrieval hits, derived memories, graph expansion, and selected-message
// neighborhoods. Graph expansion is disabled unless its budget is configured.
type SourceEvidencePacker struct {
	Base derive.ContextPacker

	SourceOnly bool

	MaxSourceMessages       int
	MaxDirectMessages       int
	MaxSummaryMessages      int
	MaxEntityFactMessages   int
	MaxGraphMessages        int
	MaxNeighborhoodMessages int
	MaxSourceRefsPerHit     int

	MinQueryTokens      int
	MinDerivedHits      int
	MinCandidates       int
	MinRelativeScore    float64
	MinEntityConfidence float64

	UseDirectMessages   bool
	UseSummaryRefs      bool
	UseEntityFactRefs   bool
	UseGraphSources     bool
	UseNeighborhood     bool
	NeighborhoodBefore  int
	NeighborhoodAfter   int
	NeighborhoodAnchors []SourceEvidenceOrigin
	GraphMaxSeedFacts   int
	GraphOptions        viewentityfact.GraphExpansionOptions

	OriginMetadataKey    string
	OriginMetadataValues SourceEvidenceOriginValues
	EvidenceMetadata     map[string]any
}

func (p SourceEvidencePacker) PackContext(ctx stdctx.Context, input derive.ContextPackInput) (derive.ContextPackOutput, error) {
	base := p.Base
	if base == nil {
		base = RRFPacker{}
	}
	output, err := base.PackContext(ctx, input)
	if err != nil {
		return derive.ContextPackOutput{}, err
	}

	budgets := p.effectiveBudgets(input.Options.SourceEvidence)
	if !budgets.enabled() {
		return output, nil
	}
	if input.SourceMessages == nil {
		return output, nil
	}

	collector := newSourceEvidenceCollector(p, input)
	if budgets.direct > 0 && p.useDirectMessages() {
		collector.collectDirect()
	}
	if budgets.summary > 0 && p.UseSummaryRefs {
		if err := collector.collectSummary(ctx); err != nil {
			return derive.ContextPackOutput{}, err
		}
	}
	if budgets.entityFact > 0 && p.UseEntityFactRefs {
		if err := collector.collectEntityFacts(ctx); err != nil {
			return derive.ContextPackOutput{}, err
		}
	}
	if budgets.graph > 0 && p.UseGraphSources {
		if err := collector.collectGraph(ctx, budgets.graph); err != nil {
			return derive.ContextPackOutput{}, err
		}
	}

	candidates := collector.ranked()
	candidates = p.filterCandidates(input, candidates)
	if len(candidates) < p.MinCandidates {
		return output, nil
	}
	selected := p.selectCandidates(candidates, budgets)
	if budgets.neighborhood > 0 && p.UseNeighborhood {
		neighbors, err := collector.collectNeighborhood(ctx, selected, budgets)
		if err != nil {
			return derive.ContextPackOutput{}, err
		}
		selected = append(selected, neighbors...)
	}

	sourceItems := make([]derive.ContextItem, 0, len(selected))
	for _, candidate := range selected {
		sourceItems = append(sourceItems, candidate.contextItem(p))
	}
	if p.SourceOnly {
		return derive.ContextPackOutput{Items: sourceItems}, nil
	}
	return derive.ContextPackOutput{Items: mergeSourceEvidenceWithBase(sourceItems, output.Items)}, nil
}

type sourceEvidenceBudgets struct {
	total        int
	direct       int
	summary      int
	entityFact   int
	graph        int
	neighborhood int
}

func (b sourceEvidenceBudgets) enabled() bool {
	return b.total > 0 || b.direct > 0 || b.summary > 0 || b.entityFact > 0 || b.graph > 0 || b.neighborhood > 0
}

func (b sourceEvidenceBudgets) limit(origin SourceEvidenceOrigin) int {
	switch origin {
	case SourceEvidenceOriginDirect:
		return b.direct
	case SourceEvidenceOriginSummary:
		return b.summary
	case SourceEvidenceOriginEntityFact:
		return b.entityFact
	case SourceEvidenceOriginGraph:
		return b.graph
	case SourceEvidenceOriginNeighborhood:
		return b.neighborhood
	default:
		return 0
	}
}

func (p SourceEvidencePacker) effectiveBudgets(opts derive.SourceEvidencePackOptions) sourceEvidenceBudgets {
	b := sourceEvidenceBudgets{
		total:        p.MaxSourceMessages,
		direct:       p.MaxDirectMessages,
		summary:      p.MaxSummaryMessages,
		entityFact:   p.MaxEntityFactMessages,
		graph:        p.MaxGraphMessages,
		neighborhood: p.MaxNeighborhoodMessages,
	}
	if opts.MaxDirectMessages > 0 {
		b.direct = opts.MaxDirectMessages
	}
	if opts.MaxSummaryMessages > 0 {
		b.summary = opts.MaxSummaryMessages
	}
	if opts.MaxEntityFactMessages > 0 {
		b.entityFact = opts.MaxEntityFactMessages
	}
	if opts.MaxGraphMessages > 0 {
		b.graph = opts.MaxGraphMessages
	}
	if opts.MaxNeighborhoodMessages > 0 {
		b.neighborhood = opts.MaxNeighborhoodMessages
	}
	if b.total <= 0 {
		b.total = positiveSum(b.direct, b.summary, b.entityFact, b.graph, b.neighborhood)
	}
	return b
}

func positiveSum(values ...int) int {
	total := 0
	for _, value := range values {
		if value > 0 {
			total += value
		}
	}
	return total
}

func (p SourceEvidencePacker) useDirectMessages() bool {
	return p.UseDirectMessages || p.MaxDirectMessages > 0
}

func (p SourceEvidencePacker) filterCandidates(input derive.ContextPackInput, candidates []sourceEvidenceCandidate) []sourceEvidenceCandidate {
	if len(candidates) == 0 {
		return nil
	}
	if p.MinQueryTokens > 0 && discriminativeQueryTokenCount(input.Query) < p.MinQueryTokens {
		return nil
	}
	if p.MinDerivedHits > 0 && derivedSourceEvidenceHitCount(input) < p.MinDerivedHits {
		return nil
	}
	if p.MinRelativeScore <= 0 {
		return candidates
	}
	maxScore := 0.0
	for _, candidate := range candidates {
		if candidate.score > maxScore {
			maxScore = candidate.score
		}
	}
	minScore := maxScore * p.MinRelativeScore
	out := candidates[:0]
	for _, candidate := range candidates {
		if candidate.hasOrigin(SourceEvidenceOriginSummary) || candidate.hasOrigin(SourceEvidenceOriginDirect) || candidate.score >= minScore {
			out = append(out, candidate)
		}
	}
	return out
}

func derivedSourceEvidenceHitCount(input derive.ContextPackInput) int {
	return len(input.SummaryHits) + len(input.EntityHits) + len(input.DocumentHits)
}

func (p SourceEvidencePacker) selectCandidates(candidates []sourceEvidenceCandidate, budgets sourceEvidenceBudgets) []sourceEvidenceCandidate {
	selected := make([]sourceEvidenceCandidate, 0, budgets.total)
	seen := map[string]bool{}
	for _, origin := range []SourceEvidenceOrigin{
		SourceEvidenceOriginSummary,
		SourceEvidenceOriginDirect,
		SourceEvidenceOriginEntityFact,
		SourceEvidenceOriginGraph,
	} {
		limit := budgets.limit(origin)
		if limit <= 0 {
			continue
		}
		added := 0
		for _, candidate := range candidates {
			if added >= limit || (budgets.total > 0 && len(selected) >= budgets.total-budgets.neighborhood) {
				break
			}
			if seen[candidate.key] || !candidate.hasOrigin(origin) {
				continue
			}
			seen[candidate.key] = true
			candidate.selectedOrigin = origin
			selected = append(selected, candidate)
			added++
		}
	}
	return selected
}

func mergeSourceEvidenceWithBase(sourceItems, baseItems []derive.ContextItem) []derive.ContextItem {
	out := make([]derive.ContextItem, 0, len(sourceItems)+len(baseItems))
	seen := map[string]bool{}
	for _, item := range sourceItems {
		if key := sourceContextItemKey(item); key != "" {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		out = append(out, item)
	}
	for _, item := range baseItems {
		if key := sourceContextItemKey(item); key != "" {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		out = append(out, item)
	}
	return out
}

type sourceEvidenceCollector struct {
	p       SourceEvidencePacker
	input   derive.ContextPackInput
	byKey   map[string]*sourceEvidenceCandidate
	ordinal int
}

func newSourceEvidenceCollector(p SourceEvidencePacker, input derive.ContextPackInput) *sourceEvidenceCollector {
	return &sourceEvidenceCollector{
		p:     p,
		input: input,
		byKey: map[string]*sourceEvidenceCandidate{},
	}
}

func (c *sourceEvidenceCollector) collectDirect() {
	for rank, hit := range c.input.MessageHits {
		score := normalizedRetrievalScore(hit.Retrieval, rank)
		c.addMessage(hit.Message, &hit.Retrieval, SourceEvidenceOriginDirect, rank, 0, score, nil)
	}
}

func (c *sourceEvidenceCollector) collectSummary(ctx stdctx.Context) error {
	for hitRank, hit := range c.input.SummaryHits {
		baseScore := normalizedRetrievalScore(hit.Retrieval, hitRank)
		for refRank, ref := range hit.Node.SourceRefs {
			if c.p.MaxSourceRefsPerHit > 0 && refRank >= c.p.MaxSourceRefsPerHit {
				break
			}
			metadata := map[string]any{
				SourceEvidenceSummaryNodeIDsMetadataKey: []string{string(hit.Node.ID)},
				"summary_node_id":                       string(hit.Node.ID),
			}
			if err := c.addSourceRef(ctx, ref, &hit.Retrieval, SourceEvidenceOriginSummary, hitRank, refRank, baseScore/float64(refRank+1), metadata); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *sourceEvidenceCollector) collectEntityFacts(ctx stdctx.Context) error {
	for hitRank, hit := range c.input.EntityHits {
		confidence := normalizedFactConfidence(hit.Fact.Confidence)
		if c.p.MinEntityConfidence > 0 && confidence < c.p.MinEntityConfidence {
			continue
		}
		baseScore := confidence / float64(hitRank+1)
		for refRank, ref := range hit.Fact.SourceRefs {
			if c.p.MaxSourceRefsPerHit > 0 && refRank >= c.p.MaxSourceRefsPerHit {
				break
			}
			metadata := map[string]any{
				SourceEvidenceEntityFactIDsMetadataKey: []string{string(hit.Fact.ID)},
				"entity_fact_id":                       string(hit.Fact.ID),
			}
			if err := c.addSourceRef(ctx, ref, &hit.Retrieval, SourceEvidenceOriginEntityFact, hitRank, refRank, baseScore/float64(refRank+1), metadata); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *sourceEvidenceCollector) collectGraph(ctx stdctx.Context, maxSource int) error {
	if c.input.EntityGraphSources == nil || len(c.input.EntityHits) == 0 || maxSource <= 0 {
		return nil
	}
	opts := c.p.GraphOptions
	if opts.MaxCandidates <= 0 {
		opts.MaxCandidates = maxSource * 4
	}
	result, err := c.input.EntityGraphSources.ExpandGraphSources(ctx, c.input.Scope, c.graphSeedFacts(), opts)
	if err != nil {
		return err
	}
	for rank, candidate := range result.Candidates {
		metadata := graphCandidateMetadata(candidate)
		hit := retrieval.Hit{
			Doc: retrieval.Doc{
				ID:       "graph:" + sourceRefDedupeKey(candidate.SourceRef),
				Metadata: maps.Clone(metadata),
			},
			Score:  candidate.Score,
			Scores: map[string]float64{"graph": candidate.Score},
		}
		if err := c.addSourceRef(ctx, candidate.SourceRef, &hit, SourceEvidenceOriginGraph, rank, 0, candidate.Score, metadata); err != nil {
			return err
		}
	}
	return nil
}

func (c *sourceEvidenceCollector) graphSeedFacts() []viewentityfact.GraphSeedFact {
	maxSeeds := c.p.GraphMaxSeedFacts
	if maxSeeds <= 0 {
		maxSeeds = 8
	}
	seeds := make([]viewentityfact.GraphSeedFact, 0, min(len(c.input.EntityHits), maxSeeds))
	for rank, hit := range c.input.EntityHits {
		if len(seeds) >= maxSeeds {
			break
		}
		if !viewentityfact.IsGraphableFact(hit.Fact) {
			continue
		}
		score := hit.Retrieval.Score
		if score <= 0 {
			score = 1 / float64(rank+1)
		}
		seeds = append(seeds, viewentityfact.GraphSeedFact{
			Fact:  hit.Fact,
			Score: score * normalizedFactConfidence(hit.Fact.Confidence),
		})
	}
	return seeds
}

func (c *sourceEvidenceCollector) collectNeighborhood(ctx stdctx.Context, selected []sourceEvidenceCandidate, budgets sourceEvidenceBudgets) ([]sourceEvidenceCandidate, error) {
	resolver, ok := c.input.SourceMessages.(derive.SourceMessageNeighborResolver)
	if !ok || c.p.NeighborhoodBefore <= 0 && c.p.NeighborhoodAfter <= 0 {
		return nil, nil
	}
	anchors := c.neighborhoodAnchors()
	seen := map[string]bool{}
	for _, candidate := range selected {
		seen[candidate.key] = true
	}
	var out []sourceEvidenceCandidate
	for _, anchor := range selected {
		if len(out) >= budgets.neighborhood {
			break
		}
		if !anchors[anchor.primaryOrigin()] || anchor.message.ConversationID == "" || anchor.message.ID == "" {
			continue
		}
		neighbors, err := resolver.GetSourceMessageNeighbors(ctx, anchor.message.ConversationID, anchor.message.ID, c.p.NeighborhoodBefore, c.p.NeighborhoodAfter)
		if err != nil {
			return nil, err
		}
		for distance, msg := range neighbors {
			if len(out) >= budgets.neighborhood || (budgets.total > 0 && len(selected)+len(out) >= budgets.total) {
				break
			}
			key := sourceMessageKey(msg)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			score := anchor.score * 0.65 / float64(distance+1)
			candidate := newSourceEvidenceCandidate(msg, nil, SourceEvidenceOriginNeighborhood, len(out), 0, score, nil, c.nextOrdinal())
			out = append(out, candidate)
		}
	}
	return out, nil
}

func (c *sourceEvidenceCollector) neighborhoodAnchors() map[SourceEvidenceOrigin]bool {
	if len(c.p.NeighborhoodAnchors) == 0 {
		return map[SourceEvidenceOrigin]bool{
			SourceEvidenceOriginSummary:    true,
			SourceEvidenceOriginDirect:     true,
			SourceEvidenceOriginEntityFact: true,
			SourceEvidenceOriginGraph:      true,
		}
	}
	out := map[SourceEvidenceOrigin]bool{}
	for _, origin := range c.p.NeighborhoodAnchors {
		if origin != "" {
			out[origin] = true
		}
	}
	return out
}

func (c *sourceEvidenceCollector) addSourceRef(ctx stdctx.Context, ref views.SourceRef, hit *retrieval.Hit, origin SourceEvidenceOrigin, hitRank, refRank int, score float64, metadata map[string]any) error {
	if ref.Kind != views.SourceMessage || ref.Message == nil {
		return nil
	}
	conversationID := ref.Message.ConversationID
	if conversationID == "" {
		conversationID = c.input.Scope.ConversationID
	}
	msg, ok, err := c.input.SourceMessages.GetSourceMessage(ctx, conversationID, ref.Message.MessageID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	c.addMessage(msg, hit, origin, hitRank, refRank, score, metadata)
	return nil
}

func (c *sourceEvidenceCollector) addMessage(msg sourcemessage.Message, hit *retrieval.Hit, origin SourceEvidenceOrigin, hitRank, refRank int, score float64, metadata map[string]any) {
	key := sourceMessageKey(msg)
	if key == "" || strings.TrimSpace(renderSourceMessageText(msg)) == "" {
		return
	}
	candidate := newSourceEvidenceCandidate(msg, hit, origin, hitRank, refRank, score, metadata, c.nextOrdinal())
	if existing, ok := c.byKey[key]; ok {
		existing.merge(candidate)
		return
	}
	c.byKey[key] = &candidate
}

func (c *sourceEvidenceCollector) nextOrdinal() int {
	c.ordinal++
	return c.ordinal
}

func (c *sourceEvidenceCollector) ranked() []sourceEvidenceCandidate {
	out := make([]sourceEvidenceCandidate, 0, len(c.byKey))
	for _, candidate := range c.byKey {
		out = append(out, *candidate)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return compareSourceEvidenceCandidate(out[i], out[j]) < 0
	})
	return out
}

type sourceEvidenceCandidate struct {
	key       string
	message   sourcemessage.Message
	hits      map[SourceEvidenceOrigin]retrieval.Hit
	metadata  map[string]any
	origins   map[SourceEvidenceOrigin]bool
	score     float64
	hitRank   int
	refRank   int
	firstSeen int

	selectedOrigin SourceEvidenceOrigin
}

func newSourceEvidenceCandidate(msg sourcemessage.Message, hit *retrieval.Hit, origin SourceEvidenceOrigin, hitRank, refRank int, score float64, metadata map[string]any, firstSeen int) sourceEvidenceCandidate {
	candidate := sourceEvidenceCandidate{
		key:       sourceMessageKey(msg),
		message:   msg,
		hits:      map[SourceEvidenceOrigin]retrieval.Hit{},
		metadata:  cloneMetadataMap(metadata),
		origins:   map[SourceEvidenceOrigin]bool{origin: true},
		score:     score,
		hitRank:   hitRank,
		refRank:   refRank,
		firstSeen: firstSeen,
	}
	if hit != nil {
		candidate.hits[origin] = retrieval.CloneHit(*hit)
	}
	return candidate
}

func (c *sourceEvidenceCandidate) merge(next sourceEvidenceCandidate) {
	for origin := range next.origins {
		c.origins[origin] = true
		if hit, ok := next.hits[origin]; ok {
			c.hits[origin] = retrieval.CloneHit(hit)
		}
	}
	if next.score > c.score || next.hitRank < c.hitRank {
		c.hitRank = next.hitRank
		c.refRank = next.refRank
	}
	c.score += next.score
	if next.firstSeen < c.firstSeen {
		c.firstSeen = next.firstSeen
	}
	mergeCandidateMetadata(c.metadata, next.metadata)
}

func (c sourceEvidenceCandidate) hasOrigin(origin SourceEvidenceOrigin) bool {
	return c.origins[origin]
}

func (c sourceEvidenceCandidate) primaryOrigin() SourceEvidenceOrigin {
	if c.selectedOrigin != "" {
		return c.selectedOrigin
	}
	for _, origin := range []SourceEvidenceOrigin{
		SourceEvidenceOriginSummary,
		SourceEvidenceOriginDirect,
		SourceEvidenceOriginEntityFact,
		SourceEvidenceOriginGraph,
		SourceEvidenceOriginNeighborhood,
	} {
		if c.origins[origin] {
			return origin
		}
	}
	return ""
}

func (c sourceEvidenceCandidate) contextItem(p SourceEvidencePacker) derive.ContextItem {
	origin := c.primaryOrigin()
	msg := c.message
	msg.Metadata = maps.Clone(msg.Metadata)
	if msg.Metadata == nil {
		msg.Metadata = map[string]any{}
	}
	applySourceEvidenceMetadata(msg.Metadata, p, origin, c.origins, c.score, c.metadata)

	item := derive.ContextItem{
		Kind:    derive.ContextItemRecentMessage,
		Text:    renderSourceMessageText(msg),
		Message: &msg,
	}
	if hit, ok := c.hits[origin]; ok {
		cloned := retrieval.CloneHit(hit)
		if cloned.Doc.Metadata == nil {
			cloned.Doc.Metadata = map[string]any{}
		}
		applySourceEvidenceMetadata(cloned.Doc.Metadata, p, origin, c.origins, c.score, c.metadata)
		item.Retrieval = &cloned
	}
	return item
}

func applySourceEvidenceMetadata(metadata map[string]any, p SourceEvidencePacker, origin SourceEvidenceOrigin, origins map[SourceEvidenceOrigin]bool, score float64, extra map[string]any) {
	metadata[SourceEvidenceOriginMetadataKey] = string(origin)
	metadata[SourceEvidenceOriginsMetadataKey] = sourceEvidenceOriginStrings(origins)
	metadata[SourceEvidenceScoreMetadataKey] = score
	originKey := p.OriginMetadataKey
	if originKey == "" {
		originKey = SourceEvidenceOriginMetadataKey
	}
	metadata[originKey] = p.originMetadataValue(origin)
	for key, value := range p.EvidenceMetadata {
		metadata[key] = value
	}
	for key, value := range extra {
		metadata[key] = cloneMetadataValue(value)
	}
}

func (p SourceEvidencePacker) originMetadataValue(origin SourceEvidenceOrigin) string {
	values := p.OriginMetadataValues
	switch origin {
	case SourceEvidenceOriginDirect:
		if values.Direct != "" {
			return values.Direct
		}
	case SourceEvidenceOriginSummary:
		if values.Summary != "" {
			return values.Summary
		}
	case SourceEvidenceOriginEntityFact:
		if values.EntityFact != "" {
			return values.EntityFact
		}
	case SourceEvidenceOriginGraph:
		if values.Graph != "" {
			return values.Graph
		}
	case SourceEvidenceOriginNeighborhood:
		if values.Neighborhood != "" {
			return values.Neighborhood
		}
	}
	return string(origin)
}

func sourceEvidenceOriginStrings(origins map[SourceEvidenceOrigin]bool) []string {
	out := make([]string, 0, len(origins))
	for _, origin := range []SourceEvidenceOrigin{
		SourceEvidenceOriginDirect,
		SourceEvidenceOriginSummary,
		SourceEvidenceOriginEntityFact,
		SourceEvidenceOriginGraph,
		SourceEvidenceOriginNeighborhood,
	} {
		if origins[origin] {
			out = append(out, string(origin))
		}
	}
	return out
}

func mergeCandidateMetadata(dst, src map[string]any) {
	for key, value := range src {
		if existing, ok := dst[key]; ok {
			dst[key] = mergeMetadataValues(existing, value)
			continue
		}
		dst[key] = cloneMetadataValue(value)
	}
}

func mergeMetadataValues(existing, next any) any {
	existingStrings := metadataStringSlice(existing)
	nextStrings := metadataStringSlice(next)
	if len(existingStrings) > 0 || len(nextStrings) > 0 {
		return appendUniqueStrings(existingStrings, nextStrings...)
	}
	return cloneMetadataValue(existing)
}

func metadataStringSlice(value any) []string {
	switch v := value.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	case []string:
		return append([]string(nil), v...)
	case []viewentityfact.FactID:
		out := make([]string, 0, len(v))
		for _, id := range v {
			if id != "" {
				out = append(out, string(id))
			}
		}
		return out
	case []viewentityfact.EntityID:
		out := make([]string, 0, len(v))
		for _, id := range v {
			if id != "" {
				out = append(out, string(id))
			}
		}
		return out
	default:
		return nil
	}
}

func appendUniqueStrings(in []string, values ...string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in)+len(values))
	for _, value := range append(in, values...) {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func cloneMetadataMap(in map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range in {
		out[key] = cloneMetadataValue(value)
	}
	return out
}

func cloneMetadataValue(value any) any {
	switch v := value.(type) {
	case []string:
		return append([]string(nil), v...)
	case []viewentityfact.FactID:
		return append([]viewentityfact.FactID(nil), v...)
	case []viewentityfact.EntityID:
		return append([]viewentityfact.EntityID(nil), v...)
	case map[string]any:
		return cloneMetadataMap(v)
	default:
		return v
	}
}

func graphCandidateMetadata(candidate viewentityfact.GraphSourceCandidate) map[string]any {
	return map[string]any{
		GraphOriginMetadataKey:  candidate.Origin,
		GraphFactIDsMetadataKey: graphFactIDs(candidate.FactIDs),
		GraphPathMetadataKey:    append([]string(nil), candidate.Paths...),
		GraphScoreMetadataKey:   candidate.Score,
		GraphSeedIDsMetadataKey: graphSeedEntityIDs(candidate.SeedEntityIDs),
	}
}

func graphFactIDs(ids []viewentityfact.FactID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			out = append(out, string(id))
		}
	}
	return out
}

func graphSeedEntityIDs(ids []viewentityfact.EntityID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			out = append(out, string(id))
		}
	}
	return out
}

func normalizedFactConfidence(confidence float64) float64 {
	if confidence <= 0 {
		return 0.8
	}
	if confidence > 1 {
		return 1
	}
	return confidence
}

func compareSourceEvidenceCandidate(a, b sourceEvidenceCandidate) int {
	if a.score > b.score {
		return -1
	}
	if a.score < b.score {
		return 1
	}
	if a.hitRank < b.hitRank {
		return -1
	}
	if a.hitRank > b.hitRank {
		return 1
	}
	if a.refRank < b.refRank {
		return -1
	}
	if a.refRank > b.refRank {
		return 1
	}
	if a.firstSeen < b.firstSeen {
		return -1
	}
	if a.firstSeen > b.firstSeen {
		return 1
	}
	return strings.Compare(a.key, b.key)
}

func renderSourceMessageText(msg sourcemessage.Message) string {
	content := msg.Content()
	if strings.TrimSpace(content) == "" {
		return ""
	}
	return string(msg.Role) + ": " + content
}

func sourceContextItemKey(item derive.ContextItem) string {
	if item.Message != nil {
		if key := sourceMessageKey(*item.Message); key != "" {
			return key
		}
	}
	if item.Retrieval != nil && strings.TrimSpace(item.Retrieval.Doc.ID) != "" {
		return "retrieval:" + strings.TrimSpace(item.Retrieval.Doc.ID)
	}
	return ""
}

func sourceMessageKey(msg sourcemessage.Message) string {
	if msg.ConversationID != "" && msg.ID != "" {
		return "message:" + msg.ConversationID + ":" + msg.ID
	}
	if msg.ID != "" {
		return "message:" + msg.ID
	}
	return ""
}

func sourceRefDedupeKey(ref views.SourceRef) string {
	if ref.Message == nil {
		return ""
	}
	if ref.Message.ConversationID != "" && ref.Message.MessageID != "" {
		return "message:" + ref.Message.ConversationID + ":" + ref.Message.MessageID
	}
	if ref.Message.MessageID != "" {
		return "message:" + ref.Message.MessageID
	}
	return ""
}

func normalizedRetrievalScore(hit retrieval.Hit, hitRank int) float64 {
	if hit.Score > 0 {
		return hit.Score / float64(hitRank+1)
	}
	return 1 / float64(hitRank+1)
}

func discriminativeQueryTokenCount(query string) int {
	seen := map[string]bool{}
	for _, raw := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		token := strings.TrimSpace(raw)
		if len(token) < 3 {
			continue
		}
		seen[token] = true
	}
	return len(seen)
}

func (p SourceEvidencePacker) String() string {
	return fmt.Sprintf("SourceEvidencePacker(max=%d)", p.MaxSourceMessages)
}
