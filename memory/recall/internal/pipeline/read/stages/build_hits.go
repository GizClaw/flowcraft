package stages

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

// BuildHits converts ranked ContextItems into Hits and optionally
// runs the reranker (legacy runRecall order: build then rerank).
type BuildHits struct {
	reranker port.Reranker
}

// NewBuildHits constructs a BuildHits stage. reranker may be nil.
func NewBuildHits(reranker port.Reranker) *BuildHits {
	return &BuildHits{reranker: reranker}
}

// Name implements pipeline.Stage.
func (BuildHits) Name() string { return "build_hits" }

// Run implements pipeline.Stage.
func (s *BuildHits) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	started := time.Now()
	hits := hitsFromItems(state.Ranked)
	state.Hits = hits
	detail := diagnostic.BuildHitsDetail{
		Count:      len(hits),
		InputCount: len(hits),
	}
	captureSnapshots := snapshotsEnabled(state)
	if captureSnapshots {
		detail.Input = candidateSnapshotPtr(hitSnapshots(hits))
		detail.Hits = candidateSnapshotPtr(hitSnapshots(hits))
	}
	if s.reranker != nil && len(hits) > 0 {
		rerankStarted := time.Now()
		reranked, err := s.reranker.Rerank(ctx, state.Query.Text, hits)
		detail.RerankLatency = time.Since(rerankStarted)
		if err != nil {
			detail.RerankErr = err.Error()
		} else {
			hits = reranked
			state.Hits = hits
			detail.Reranked = len(hits)
			if captureSnapshots {
				detail.RerankedHits = candidateSnapshotPtr(hitSnapshots(hits))
			}
		}
	}
	if state.Plan != nil && state.Plan.TotalCap > 0 {
		finalSelectionStarted := time.Now()
		poolHits := hitsFromItems(finalSelectionPool(state))
		hits = selectFinalEvidenceAwareHits(state.Query.Text, hits, poolHits, state.Plan.TotalCap)
		detail.FinalSelectionLatency = time.Since(finalSelectionStarted)
		state.Hits = hits
	}
	hits = groundHitsWithSupportingEvidence(state.Query.Text, hits)
	state.Hits = hits
	detail.Count = len(hits)
	if captureSnapshots {
		detail.Hits = candidateSnapshotPtr(hitSnapshots(hits))
	}
	detail.Latency = time.Since(started)
	return detail, nil
}

func finalSelectionPool(state *read.ReadState) []domain.ContextItem {
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

func groundHitsWithSupportingEvidence(query string, hits []domain.Hit) []domain.Hit {
	if len(hits) == 0 {
		return hits
	}
	out := make([]domain.Hit, len(hits))
	for i, hit := range hits {
		hit.Evidence = selectGroundingEvidence(query, hit.Evidence, hit.Fact.EvidenceRefs)
		out[i] = hit
	}
	return out
}

func selectGroundingEvidence(query string, selected []domain.EvidenceRef, refs []domain.EvidenceRef) []domain.EvidenceRef {
	out := make([]domain.EvidenceRef, 0, maxHitEvidenceRefs)
	seen := map[string]struct{}{}
	appendRef := func(ref domain.EvidenceRef) {
		if len(out) >= maxHitEvidenceRefs || strings.TrimSpace(ref.Text) == "" {
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
	if len(out) >= maxHitEvidenceRefs || len(refs) == 0 {
		return out
	}
	queryFeatures := newGroundingQueryFeatures(query)
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
		score, eligible := groundingEvidenceScore(queryFeatures, ref)
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

func newGroundingQueryFeatures(query string) groundingQueryFeatures {
	return groundingQueryFeatures{
		tokens:           groundingTokenSet(query),
		numeric:          numericTokens(query),
		proper:           properNounSet(query),
		hasTimeSignal:    queryHasTimeSignal(query),
		hasNumericIntent: queryHasNumericIntent(query),
	}
}

func groundingEvidenceScore(query groundingQueryFeatures, ref domain.EvidenceRef) (float64, bool) {
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
	numericMatch := intersects(query.numeric, numericTokens(text))
	timeMatch := query.hasTimeSignal && (!ref.Timestamp.IsZero() || hasTimex(text, time.Now()))
	properMatch := intersects(query.proper, properNounSet(text))
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
	return tokenSet(tokenize.Detect(text).Tokenize(text))
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

var _ pipeline.Stage[*read.ReadState] = (*BuildHits)(nil)
