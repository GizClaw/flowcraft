package stages

import (
	"context"
	"slices"
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
		detail.Input = candidateSnapshotPtr(hitSnapshots(hits))
		detail.Hits = candidateSnapshotPtr(hitSnapshots(hits))
	}
	hits, err := s.rerankHits(ctx, state.Query.Text, hits, &detail, captureSnapshots)
	if err != nil {
		detail.Latency = time.Since(started)
		return detail, err
	}
	state.Hits = hits
	hits = packContextPackSelection(state, rankOutputHits, hits, &detail)
	state.Hits = hits
	detail.Count = len(hits)
	if captureSnapshots {
		detail.Hits = candidateSnapshotPtr(hitSnapshots(hits))
	}
	detail.Latency = time.Since(started)
	return detail, nil
}

func (s *ContextPack) rerankHits(ctx context.Context, query string, hits []domain.Hit, detail *diagnostic.ContextPackDetail, captureSnapshots bool) ([]domain.Hit, error) {
	if s.reranker == nil || len(hits) == 0 {
		return hits, nil
	}
	rerankStarted := time.Now()
	reranked, err := s.reranker.Rerank(ctx, query, hits)
	detail.RerankLatency = time.Since(rerankStarted)
	if err != nil {
		detail.RerankErr = err.Error()
		if isContextError(err) {
			return hits, err
		}
		return hits, nil
	}
	detail.Reranked = len(reranked)
	if captureSnapshots {
		detail.RerankedHits = candidateSnapshotPtr(hitSnapshots(reranked))
	}
	return reranked, nil
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
		detail.Input = candidateSnapshotPtr(hitSnapshots(hits))
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
		detail.Hits = candidateSnapshotPtr(hitSnapshots(hits))
	}
	return detail, nil
}

func (s *BuildGroundedHits) buildEvidencePacket(ctx context.Context, queryScope domain.Scope, hit domain.Hit) domain.EvidencePacket {
	packet := domain.EvidencePacket{
		Primary:      hit.Ref,
		EvidenceRefs: append([]domain.EvidenceRef(nil), hit.Evidence...),
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
	return hitsFromItems(state.Ranked)
}

func packContextPackSelection(state *read.ReadState, rankOutputHits, hits []domain.Hit, detail *diagnostic.ContextPackDetail) []domain.Hit {
	if state == nil || state.Plan == nil || state.Plan.TotalCap <= 0 {
		return hits
	}
	contextPackingStarted := time.Now()
	poolHits := hitsFromItems(contextPackingPool(state))
	if len(hits) > 0 {
		poolHits = mergeContextPackPool(hits, poolHits)
	}
	out, trace := packRecallContextWithIntentTrace(state.Plan.Intent, rankOutputHits, poolHits, state.Plan.TotalCap)
	out = packTaskAwareEvidenceClusters(state.Plan.TaskIntents, out, poolHits, state.Plan.TotalCap)
	detail.ContextPackingLatency = time.Since(contextPackingStarted)
	if len(trace) > 0 {
		detail.PackTrace = candidateSnapshotPtr(trace)
	}
	return out
}

func packTaskAwareEvidenceClusters(tasks []domain.QueryTaskIntent, selected, pool []domain.Hit, cap int) []domain.Hit {
	if cap <= 0 || len(tasks) == 0 || len(pool) == 0 {
		return selected
	}
	out := append([]domain.Hit(nil), selected...)
	for _, task := range tasks {
		switch task {
		case domain.QueryTaskYesNoVerification, domain.QueryTaskAbsenceCheck:
			out = ensureTaskEvidence(out, pool, cap, func(h domain.Hit) bool {
				f := domain.NormalizeSemantic(h.Fact)
				return f.Polarity == domain.PolarityNegated ||
					f.Modality == domain.ModalityCanceled ||
					f.Modality == domain.ModalityPlanned ||
					f.Modality == domain.ModalityCounterfactual
			})
		}
	}
	return out
}

func ensureTaskEvidence(selected, pool []domain.Hit, cap int, match func(domain.Hit) bool) []domain.Hit {
	if match == nil || taskEvidenceCovered(selected, match) {
		return selected
	}
	for _, hit := range pool {
		if hitAlreadySelected(selected, hit) || !match(hit) {
			continue
		}
		if len(selected) < cap {
			return append(selected, hit)
		}
		replace := taskEvidenceReplacementIndex(selected)
		if replace >= 0 {
			out := append([]domain.Hit(nil), selected...)
			out[replace] = hit
			sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
			return out
		}
	}
	return selected
}

func taskEvidenceCovered(hits []domain.Hit, match func(domain.Hit) bool) bool {
	return slices.ContainsFunc(hits, match)
}

func hitAlreadySelected(hits []domain.Hit, candidate domain.Hit) bool {
	for _, hit := range hits {
		if hit.Fact.ID != "" && hit.Fact.ID == candidate.Fact.ID {
			return true
		}
	}
	return false
}

func taskEvidenceReplacementIndex(hits []domain.Hit) int {
	idx := -1
	var score float64
	for i, hit := range hits {
		f := domain.NormalizeSemantic(hit.Fact)
		if f.Polarity == domain.PolarityNegated ||
			f.Modality != domain.ModalityActual {
			continue
		}
		if idx < 0 || hit.Score < score {
			idx = i
			score = hit.Score
		}
	}
	return idx
}

func contextPackNow(state *read.ReadState) time.Time {
	if state != nil && !state.Now.IsZero() {
		return state.Now
	}
	return time.Now()
}

func contextPackingPool(state *read.ReadState) []domain.ContextItem {
	if state == nil {
		return nil
	}
	if len(state.AfterTrust) > 0 {
		return state.AfterTrust
	}
	if state.PolicyFiltered {
		return state.AfterTrust
	}
	if len(state.MergedItems) > 0 {
		return state.MergedItems
	}
	read.PromoteMergedItems(state)
	return state.MergedItems
}

func hitsFromItems(items []domain.ContextItem) []domain.Hit {
	hits := make([]domain.Hit, 0, len(items))
	for _, it := range items {
		hits = append(hits, domain.Hit{
			Ref:         contextItemRef(it),
			Fact:        it.Fact,
			Observation: it.Observation,
			Link:        it.Link,
			Evidence:    append([]domain.EvidenceRef(nil), it.Evidence...),
			Score:       it.Candidate.Score,
			Sources:     hitSources(it.Candidate),
		})
	}
	return hits
}

func contextItemRef(item domain.ContextItem) domain.CandidateRef {
	if item.Ref.ID != "" || item.Ref.Kind != "" {
		return item.Ref
	}
	return item.Candidate
}

const maxHitEvidenceRefs = 3

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
