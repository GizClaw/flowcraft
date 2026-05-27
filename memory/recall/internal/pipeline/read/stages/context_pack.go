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
	features := queryFeaturesFromState(state)
	now := contextPackNow(state)
	hits := contextPackInitialHits(state)
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
	hits = packContextPackSelection(state, features, now, hits, &detail)
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
// supporting refs to each already-packed hit.
type BuildGroundedHits struct{}

func NewBuildGroundedHits() *BuildGroundedHits { return &BuildGroundedHits{} }

func (BuildGroundedHits) Name() string { return "build_grounded_hits" }

func (BuildGroundedHits) Run(_ context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	started := time.Now()
	features := queryFeaturesFromState(state)
	now := contextPackNow(state)
	hits := state.Hits
	detail := diagnostic.BuildGroundedHitsDetail{InputCount: len(hits)}
	if snapshotsEnabled(state) {
		detail.Input = candidateSnapshotPtr(hitSnapshots(hits))
	}
	hits = groundHitsWithSupportingEvidence(features, now, hits)
	state.Hits = hits
	detail.Count = len(hits)
	detail.Latency = time.Since(started)
	if snapshotsEnabled(state) {
		detail.Hits = candidateSnapshotPtr(hitSnapshots(hits))
	}
	return detail, nil
}

func contextPackInitialHits(state *read.ReadState) []domain.Hit {
	if state == nil {
		return nil
	}
	return hitsFromItems(state.Ranked)
}

func packContextPackSelection(state *read.ReadState, features domain.QueryFeatures, now time.Time, hits []domain.Hit, detail *diagnostic.ContextPackDetail) []domain.Hit {
	if state == nil || state.Plan == nil || state.Plan.TotalCap <= 0 {
		return hits
	}
	contextPackingStarted := time.Now()
	poolHits := hitsFromItems(contextPackingPool(state))
	out := packRecallContextWithFeatures(state.Query.Text, features, now, hits, poolHits, state.Plan.TotalCap)
	detail.ContextPackingLatency = time.Since(contextPackingStarted)
	return out
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
			Fact:     it.Fact,
			Evidence: append([]domain.EvidenceRef(nil), it.Evidence...),
			Score:    it.Candidate.Score,
			Sources:  hitSources(it.Candidate),
		})
	}
	return hits
}

const maxHitEvidenceRefs = 3

type groundingQueryFeatures struct {
	tokens           map[string]struct{}
	numeric          map[string]struct{}
	proper           map[string]struct{}
	hasTimeSignal    bool
	hasNumericIntent bool
}

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

func selectGroundingEvidence(query string, selected []domain.EvidenceRef, refs []domain.EvidenceRef) []domain.EvidenceRef {
	return selectGroundingEvidenceWithFeatures(recallintent.ExtractFeatures(query), time.Now(), selected, refs)
}

func selectGroundingEvidenceWithFeatures(features domain.QueryFeatures, now time.Time, selected []domain.EvidenceRef, refs []domain.EvidenceRef) []domain.EvidenceRef {
	if now.IsZero() {
		now = time.Now()
	}
	limit := maxGroundingEvidenceRefs(features)
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
	queryFeatures := newGroundingQueryFeatures(features)
	if len(queryFeatures.tokens) == 0 && len(queryFeatures.numeric) == 0 && len(queryFeatures.proper) == 0 && !queryFeatures.hasTimeSignal {
		return out
	}
	type scoredRef struct {
		ref   domain.EvidenceRef
		score float64
		rank  int
	}
	candidates := make([]scoredRef, 0, len(refs))
	for i, ref := range refs {
		if strings.TrimSpace(ref.Text) == "" {
			continue
		}
		if key := evidenceRefKey(ref); key != "" {
			if _, ok := seen[key]; ok {
				continue
			}
		}
		score, eligible := groundingEvidenceScore(queryFeatures, now, ref)
		if !eligible {
			continue
		}
		candidates = append(candidates, scoredRef{ref: ref, score: score, rank: i})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].rank < candidates[j].rank
	})
	for _, cand := range candidates {
		appendRef(cand.ref)
	}
	return out
}

func maxGroundingEvidenceRefs(features domain.QueryFeatures) int {
	if features.HasTimeSignal() || features.NumericIntent {
		return maxHitEvidenceRefs + 1
	}
	return maxHitEvidenceRefs
}

func newGroundingQueryFeatures(features domain.QueryFeatures) groundingQueryFeatures {
	return groundingQueryFeatures{
		tokens:           features.Tokens,
		numeric:          features.Numeric,
		proper:           features.Proper,
		hasTimeSignal:    features.HasTimeSignal(),
		hasNumericIntent: features.NumericIntent,
	}
}

func groundingEvidenceScore(query groundingQueryFeatures, now time.Time, ref domain.EvidenceRef) (float64, bool) {
	text := ref.Text
	tokens := groundingTokenSet(text)
	matched := 0
	for tok := range query.tokens {
		if _, ok := tokens[tok]; ok {
			matched++
		}
	}
	coverage := 0.0
	if len(query.tokens) > 0 {
		coverage = float64(matched) / float64(len(query.tokens))
	}
	numericMatch := intersects(query.numeric, recallintent.NumericTokens(text))
	timeMatch := query.hasTimeSignal && (!ref.Timestamp.IsZero() || recallintent.HasTimex(text, now))
	properMatch := intersects(query.proper, recallintent.ProperNounSet(text))
	if len(query.tokens) == 0 && (numericMatch || timeMatch || (properMatch && !query.hasTimeSignal && !query.hasNumericIntent)) {
		score := 0.40
		if numericMatch {
			score += 0.25
		}
		if timeMatch {
			score += 0.20
		}
		if properMatch {
			score += 0.10
		}
		if score > 1 {
			score = 1
		}
		return score, true
	}
	eligible := matched >= 2 ||
		(matched >= 1 && query.hasTimeSignal && timeMatch) ||
		(matched >= 1 && query.hasNumericIntent && numericMatch)
	if !eligible {
		return 0, false
	}
	score := coverage
	if numericMatch || timeMatch {
		score += 0.20
	}
	if properMatch {
		score += 0.05
	}
	if score > 1 {
		score = 1
	}
	return score, true
}

func groundingTokenSet(text string) map[string]struct{} {
	return recallintent.TextTokenSet(text)
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
