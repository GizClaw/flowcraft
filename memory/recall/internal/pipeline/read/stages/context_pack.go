package stages

import (
	"context"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	recallintent "github.com/GizClaw/flowcraft/memory/recall/internal/intent"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// ContextPack converts ranked ContextItems into Hits, applies the optional
// reranker, and fits the result into the final context budget.
type ContextPack struct {
	reranker port.Reranker
}

func NewContextPack(reranker port.Reranker) *ContextPack {
	return &ContextPack{reranker: reranker}
}

func (ContextPack) Name() string { return "context_pack" }

func (s *ContextPack) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	started := time.Now()
	hits := contextPackInitialHits(state)
	rankOutputHits := append([]domain.Hit(nil), hits...)
	state.Hits = hits
	detail := diagnostic.ContextPackDetail{Count: len(hits), InputCount: len(hits)}
	captureSnapshots := snapshotsEnabled(state)
	if captureSnapshots {
		detail.Input = candidateSnapshotPtr(hitSnapshotsWithScoreLabel(hits, scoreLabelRank))
		detail.Hits = candidateSnapshotPtr(hitSnapshotsWithScoreLabel(hits, scoreLabelFinal))
	}
	hits, reranked, err := s.rerankHits(ctx, state, hits, &detail, captureSnapshots)
	if err != nil {
		detail.Latency = time.Since(started)
		return detail, err
	}
	state.Hits = hits
	packOrderedHits := rankOutputHits
	if reranked || len(hits) > 0 {
		packOrderedHits = append([]domain.Hit(nil), hits...)
	}
	hits = packContextPackSelection(state, packOrderedHits, rankOutputHits, hits, reranked, &detail)
	state.Hits = hits
	recordContextPackPackedHits(state, hits)
	detail.Count = len(hits)
	if captureSnapshots {
		detail.Hits = candidateSnapshotPtr(hitSnapshotsWithScoreLabel(hits, scoreLabelFinal))
	}
	detail.Latency = time.Since(started)
	return detail, nil
}

func (s *ContextPack) rerankHits(ctx context.Context, state *read.ReadState, hits []domain.Hit, detail *diagnostic.ContextPackDetail, captureSnapshots bool) ([]domain.Hit, bool, error) {
	if s.reranker == nil || len(hits) == 0 {
		return hits, false, nil
	}
	rerankStarted := time.Now()
	intent := contextPackIntent(state)
	var reranked []domain.Hit
	var err error
	if structured, ok := s.reranker.(port.IntentReranker); ok {
		reranked, err = structured.RerankWithIntent(ctx, intent, hits)
	} else {
		reranked, err = s.reranker.Rerank(ctx, canonicalRerankQuery(intent), hits)
	}
	detail.RerankLatency = time.Since(rerankStarted)
	if err != nil {
		detail.RerankErr = err.Error()
		if isContextError(err) {
			return hits, false, err
		}
		return hits, false, nil
	}
	detail.Reranked = len(reranked)
	reranked = filterContextPackRerankedHits(hits, reranked)
	detail.Reranked = len(reranked)
	if captureSnapshots {
		detail.RerankedHits = candidateSnapshotPtr(hitSnapshotsWithScoreLabel(reranked, scoreLabelRank))
	}
	return reranked, len(reranked) > 0, nil
}

func filterContextPackRerankedHits(input []domain.Hit, reranked []domain.Hit) []domain.Hit {
	if len(reranked) == 0 {
		return nil
	}
	allowed := contextPackAllowedHitKeys(input)
	if len(allowed) == 0 {
		return nil
	}
	out := make([]domain.Hit, 0, len(reranked))
	seen := make(map[string]struct{}, len(reranked))
	for _, hit := range reranked {
		key := contextPackHitKey(hit)
		if key == "" {
			continue
		}
		if _, ok := allowed[key]; !ok {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, hit)
	}
	return out
}

func contextPackAllowedHitKeys(input []domain.Hit) map[string]struct{} {
	allowed := make(map[string]struct{}, len(input))
	add := func(hit domain.Hit) {
		if key := contextPackHitKey(hit); key != "" {
			allowed[key] = struct{}{}
		}
	}
	for _, hit := range input {
		add(hit)
	}
	return allowed
}

func contextPackHitKey(hit domain.Hit) string {
	if hit.Ref.Kind != "" && hit.Ref.ID != "" {
		return string(hit.Ref.Kind) + ":" + hit.Ref.ID
	}
	if hit.Fact.ID != "" {
		return string(domain.GraphNodeAssertion) + ":" + hit.Fact.ID
	}
	if hit.Observation.ID != "" {
		return string(domain.GraphNodeObservation) + ":" + hit.Observation.ID
	}
	if hit.Link.ID != "" {
		return string(domain.GraphNodeLink) + ":" + hit.Link.ID
	}
	return ""
}

func contextPackIntent(state *read.ReadState) domain.QueryIntent {
	if state != nil && state.Plan != nil {
		return state.Plan.Intent
	}
	if state != nil && state.Intent != nil {
		return *state.Intent
	}
	if state != nil {
		return domain.QueryIntent{Text: state.Query.Text, Entities: state.Query.Entities, Subject: state.Query.Subject, Predicate: state.Query.Predicate, Object: state.Query.Object}
	}
	return domain.QueryIntent{}
}

func canonicalRerankQuery(intent domain.QueryIntent) string {
	parts := appendNonEmpty(nil, intent.Text, intent.Subject, intent.Predicate, intent.Object)
	parts = append(parts, intent.Entities...)
	for _, kind := range intent.Kinds {
		if kind != "" {
			parts = append(parts, string(kind))
		}
	}
	if !intent.TimeRange.IsZero() {
		if !intent.TimeRange.From.IsZero() {
			parts = append(parts, intent.TimeRange.From.Format("2006-01-02"))
		}
		if !intent.TimeRange.To.IsZero() {
			parts = append(parts, intent.TimeRange.To.Format("2006-01-02"))
		}
	}
	return strings.Join(parts, " ")
}

func appendNonEmpty(out []string, values ...string) []string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

// BuildGroundedHits finalizes recall output by adding query-relevant
// supporting refs and a canonical O/A/L evidence packet to each packed hit.
type BuildGroundedHits struct {
	observations port.ObservationStore
	links        port.LinkStore
}

type BuildGroundedHitsOption func(*BuildGroundedHits)

func WithGroundedHitGraph(observations port.ObservationStore, links port.LinkStore) BuildGroundedHitsOption {
	return func(s *BuildGroundedHits) {
		s.observations = observations
		s.links = links
	}
}

func NewBuildGroundedHits(opts ...BuildGroundedHitsOption) *BuildGroundedHits {
	s := &BuildGroundedHits{}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

func (BuildGroundedHits) Name() string { return "build_grounded_hits" }

func (s *BuildGroundedHits) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	started := time.Now()
	if state == nil {
		return diagnostic.BuildGroundedHitsDetail{Latency: time.Since(started)}, nil
	}
	features := queryFeaturesFromState(state)
	now := contextPackNow(state)
	hits := state.Hits
	detail := diagnostic.BuildGroundedHitsDetail{InputCount: len(hits)}
	if snapshotsEnabled(state) {
		detail.Input = candidateSnapshotPtr(hitSnapshotsWithScoreLabel(hits, scoreLabelFinal))
	}
	hits = groundHitsWithSupportingEvidence(features, now, hits)
	queryScope := state.Scope
	for i := range hits {
		hits[i].EvidencePacket = s.buildEvidencePacket(ctx, queryScope, hits[i])
		hits[i].AnswerEvidence = domain.BuildEvidenceTable([]domain.Hit{hits[i]})
	}
	state.Hits = hits
	detail.Count = len(hits)
	detail.Latency = time.Since(started)
	if snapshotsEnabled(state) {
		detail.Hits = candidateSnapshotPtr(hitSnapshotsWithScoreLabel(hits, scoreLabelFinal))
	}
	return detail, nil
}

func (s *BuildGroundedHits) buildEvidencePacket(ctx context.Context, queryScope domain.Scope, hit domain.Hit) domain.EvidencePacket {
	packet := domain.EvidencePacket{
		Primary:      hit.Ref,
		EvidenceRefs: cappedEvidenceRefs(hit.Evidence, maxHitEvidenceRefs),
	}
	if packet.Primary.ID == "" && packet.Primary.Kind == "" {
		packet.Primary = domain.CandidateRef{Kind: domain.GraphNodeAssertion, ID: hit.Fact.ID, Scope: hit.Fact.Scope}
		if hit.Observation.ID != "" {
			packet.Primary = domain.CandidateRef{Kind: domain.GraphNodeObservation, ID: hit.Observation.ID, Scope: hit.Observation.Scope}
		}
	}
	if hit.Fact.ID != "" {
		packet.Assertions = append(packet.Assertions, hit.Fact.Clone())
	}
	if hit.Observation.ID != "" && (queryScope.RuntimeID == "" || domain.ScopeVisible(queryScope, hit.Observation.Scope)) {
		packet.Observations = append(packet.Observations, hit.Observation.Clone())
	}
	if hit.Link.ID != "" && (queryScope.RuntimeID == "" || domain.ScopeVisible(queryScope, hit.Link.Scope)) {
		packet.Links = append(packet.Links, hit.Link.Clone())
	}
	if s == nil {
		return packet
	}
	seenObs := map[string]struct{}{}
	for _, obs := range packet.Observations {
		if obs.ID != "" {
			seenObs[obs.ID] = struct{}{}
		}
	}
	if s.observations != nil {
		for _, ref := range hit.Evidence {
			if len(packet.Observations) >= maxEvidencePacketObservations {
				break
			}
			if ref.ObservationID == "" {
				continue
			}
			if _, ok := seenObs[ref.ObservationID]; ok {
				continue
			}
			obs, err := s.observations.Get(ctx, packetScope(hit), ref.ObservationID)
			if err != nil {
				continue
			}
			if queryScope.RuntimeID != "" && !domain.ScopeVisible(queryScope, obs.Scope) {
				continue
			}
			packet.Observations = append(packet.Observations, obs.Clone())
			seenObs[obs.ID] = struct{}{}
		}
	}
	if s.links != nil && hit.Fact.ID != "" {
		links, err := s.links.FindByNode(ctx, hit.Fact.Scope, domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: hit.Fact.ID})
		if err == nil {
			seenLinks := map[string]struct{}{}
			for _, link := range packet.Links {
				if link.ID != "" {
					seenLinks[link.ID] = struct{}{}
				}
			}
			for _, link := range links {
				if len(packet.Links) >= maxEvidencePacketLinks {
					break
				}
				if queryScope.RuntimeID != "" && !domain.ScopeVisible(queryScope, link.Scope) {
					continue
				}
				if link.ID == "" {
					continue
				}
				if _, ok := seenLinks[link.ID]; ok {
					continue
				}
				packet.Links = append(packet.Links, link.Clone())
				seenLinks[link.ID] = struct{}{}
				if s.observations != nil {
					for _, ref := range link.EvidenceRefs {
						if len(packet.Observations) >= maxEvidencePacketObservations {
							break
						}
						if ref.ObservationID == "" {
							continue
						}
						if _, ok := seenObs[ref.ObservationID]; ok {
							continue
						}
						obs, err := s.observations.Get(ctx, hit.Fact.Scope, ref.ObservationID)
						if err != nil {
							continue
						}
						if queryScope.RuntimeID != "" && !domain.ScopeVisible(queryScope, obs.Scope) {
							continue
						}
						packet.Observations = append(packet.Observations, obs.Clone())
						seenObs[obs.ID] = struct{}{}
					}
				}
			}
		}
	}
	return packet
}

func cappedEvidenceRefs(refs []domain.EvidenceRef, limit int) []domain.EvidenceRef {
	if limit <= 0 || len(refs) <= limit {
		return append([]domain.EvidenceRef(nil), refs...)
	}
	return append([]domain.EvidenceRef(nil), refs[:limit]...)
}

func packetScope(hit domain.Hit) domain.Scope {
	if hit.Fact.Scope.RuntimeID != "" {
		return hit.Fact.Scope
	}
	if hit.Observation.Scope.RuntimeID != "" {
		return hit.Observation.Scope
	}
	if hit.Link.Scope.RuntimeID != "" {
		return hit.Link.Scope
	}
	return hit.Ref.Scope
}

func contextPackInitialHits(state *read.ReadState) []domain.Hit {
	if state == nil {
		return nil
	}
	return hitsFromItems(state, state.Ranked)
}

func packContextPackSelection(state *read.ReadState, orderedHits, rankOutputHits, hits []domain.Hit, reranked bool, detail *diagnostic.ContextPackDetail) []domain.Hit {
	if state == nil || state.Plan == nil || state.Plan.TotalCap <= 0 {
		return hits
	}
	contextPackingStarted := time.Now()
	poolHits := hitsFromItems(state, contextPackingPool(state))
	if reranked {
		poolHits = mergeContextPackPool(rankOutputHits, poolHits)
	}
	if len(hits) > 0 {
		poolHits = mergeContextPackPool(hits, poolHits)
	}
	var out []domain.Hit
	var trace []diagnostic.CandidateSnapshot
	if reranked {
		out, trace = packRerankedRecallContextWithIntentTrace(state.Plan.Intent, orderedHits, poolHits, state.Plan.TotalCap)
	} else {
		out, trace = packRecallContextWithIntentTrace(state.Plan.Intent, orderedHits, poolHits, state.Plan.TotalCap)
	}
	detail.ContextPackingLatency = time.Since(contextPackingStarted)
	if len(trace) > 0 {
		detail.PackTrace = candidateSnapshotPtr(trace)
		recordContextPackTraceDecisions(state, trace)
	}
	return out
}

func contextPackNow(state *read.ReadState) time.Time {
	if state != nil && !state.Now.IsZero() {
		return state.Now
	}
	return time.Now()
}

func contextPackingPool(state *read.ReadState) []domain.ContextItem {
	if state == nil || !state.AssessmentApplied {
		return nil
	}
	return state.AssessedItems
}

func hitsFromItems(state *read.ReadState, items []domain.ContextItem) []domain.Hit {
	hits := make([]domain.Hit, 0, len(items))
	for _, it := range items {
		hits = append(hits, domain.Hit{
			Ref:         contextItemRef(it),
			Fact:        it.Fact,
			Observation: it.Observation,
			Link:        it.Link,
			Evidence:    append([]domain.EvidenceRef(nil), it.Evidence...),
			Score:       contextItemHitScore(state, it),
			Sources:     hitSources(it.Candidate),
		})
	}
	return hits
}

func contextItemHitScore(state *read.ReadState, item domain.ContextItem) float64 {
	if score, ok := state.CandidateRankScore(item); ok {
		return score
	}
	if score, ok := state.CandidateAssessmentScore(item); ok {
		return score
	}
	return 0
}

func recordContextPackPackedHits(state *read.ReadState, hits []domain.Hit) {
	if state == nil {
		return
	}
	for i, hit := range hits {
		if id := contextPackTraceFactID(hit); id != "" {
			state.RecordCandidatePack(id, domain.PackDecision{Packed: true, Reason: "packed", OutputRank: i + 1})
		}
	}
}

func recordContextPackTraceDecisions(state *read.ReadState, trace []diagnostic.CandidateSnapshot) {
	if state == nil {
		return
	}
	for _, snap := range trace {
		if snap.FactID == "" {
			continue
		}
		decision := domain.PackDecision{
			Packed:     snap.ContextPackRank > 0,
			Reason:     snap.DroppedReason,
			InputRank:  snap.RankOutputRank,
			OutputRank: snap.ContextPackRank,
		}
		if decision.Packed && decision.Reason == "" {
			decision.Reason = "packed"
		}
		state.RecordCandidatePack(snap.FactID, decision)
	}
}

func contextItemRef(item domain.ContextItem) domain.CandidateRef {
	if item.Ref.ID != "" || item.Ref.Kind != "" {
		return item.Ref
	}
	return item.Candidate
}

const (
	maxHitEvidenceRefs            = 3
	maxEvidencePacketLinks        = 8
	maxEvidencePacketObservations = 4
)

func queryFeaturesFromState(state *read.ReadState) domain.QueryFeatures {
	if state != nil && state.Intent != nil && !state.Intent.Features.IsZero() {
		return state.Intent.Features
	}
	if state != nil && state.Plan != nil && !state.Plan.Intent.Features.IsZero() {
		return state.Plan.Intent.Features
	}
	if state != nil {
		return recallintent.ExtractFeatures(state.Query.Text)
	}
	return domain.QueryFeatures{}
}

func groundHitsWithSupportingEvidence(features domain.QueryFeatures, now time.Time, hits []domain.Hit) []domain.Hit {
	if len(hits) == 0 {
		return hits
	}
	out := make([]domain.Hit, len(hits))
	for i, hit := range hits {
		hit.Evidence = selectGroundingEvidenceWithFeatures(features, now, hit.Evidence, hit.Fact.EvidenceRefs)
		out[i] = hit
	}
	return out
}

func selectGroundingEvidence(_ string, selected []domain.EvidenceRef, refs []domain.EvidenceRef) []domain.EvidenceRef {
	return selectGroundingEvidenceWithFeatures(domain.QueryFeatures{}, time.Time{}, selected, refs)
}

func selectGroundingEvidenceWithFeatures(_ domain.QueryFeatures, _ time.Time, selected []domain.EvidenceRef, refs []domain.EvidenceRef) []domain.EvidenceRef {
	limit := maxHitEvidenceRefs
	out := make([]domain.EvidenceRef, 0, limit)
	seen := map[string]struct{}{}
	appendRef := func(ref domain.EvidenceRef) {
		if len(out) >= limit || strings.TrimSpace(ref.Text) == "" {
			return
		}
		key := evidenceRefKey(ref)
		if key != "" {
			if _, ok := seen[key]; ok {
				return
			}
			seen[key] = struct{}{}
		}
		out = append(out, ref)
	}
	for _, ref := range selected {
		appendRef(ref)
	}
	if len(out) >= limit || len(refs) == 0 {
		return out
	}
	for _, ref := range refs {
		appendRef(ref)
	}
	return out
}

func evidenceRefKey(ref domain.EvidenceRef) string {
	if ref.ID != "" {
		return "id:" + ref.ID
	}
	if ref.MessageID != "" {
		return "msg:" + ref.MessageID
	}
	text := strings.ToLower(strings.Join(strings.Fields(ref.Text), " "))
	if text != "" {
		return "text:" + text
	}
	return ""
}

func hitSources(c domain.Candidate) []string {
	if c.Metadata != nil {
		if existing, ok := c.Metadata["sources"].([]string); ok && len(existing) > 0 {
			out := make([]string, len(existing))
			copy(out, existing)
			return out
		}
	}
	if c.Source != "" {
		return []string{c.Source}
	}
	return nil
}

var (
	_ pipeline.Stage[*read.ReadState] = (*ContextPack)(nil)
	_ pipeline.Stage[*read.ReadState] = (*BuildGroundedHits)(nil)
)
