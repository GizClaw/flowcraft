package stages

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

const (
	contextPackSameEvidenceSiblingCap = 2
	contextPackBaseScoreWeight        = 0.85
	contextPackRankPriorWeight        = 0.15

	contextPackSourcePenaltyPerExtra = 0.03
	contextPackSourcePenaltyMax      = 0.12
	contextPackDiversityPenaltyScale = 0.22
	contextPackDiversityPenaltyFloor = 0.35

	contextPackObservationDefaultCap = 1
	contextPackObservationOnlyCap    = 4

	contextPackRankOutputAnchorDivisor = 3
)

type contextPackInput struct {
	cap int
}

type contextPackCandidate struct {
	hit           domain.Hit
	score         float64
	baseScore     float64
	queryRank     int
	evidenceKey   string
	evidenceGroup string
}

func packRecallContextWithFeaturesAndDetail(ordered []domain.Hit, pool []domain.Hit, cap int) []domain.Hit {
	hits, _ := packRecallContextWithTrace(ordered, pool, cap)
	return hits
}

func packRecallContextWithTrace(ordered []domain.Hit, pool []domain.Hit, cap int) ([]domain.Hit, []diagnostic.CandidateSnapshot) {
	return packRecallContextWithIntentTrace(domain.QueryIntent{}, ordered, pool, cap)
}

func packRecallContextWithIntentTrace(intent domain.QueryIntent, ordered []domain.Hit, pool []domain.Hit, cap int) ([]domain.Hit, []diagnostic.CandidateSnapshot) {
	return packRecallContextWithIntentTraceAndAnchorCap(intent, ordered, pool, cap, contextPackRankOutputAnchorCount(cap))
}

func packRerankedRecallContextWithIntentTrace(intent domain.QueryIntent, ordered []domain.Hit, pool []domain.Hit, cap int) ([]domain.Hit, []diagnostic.CandidateSnapshot) {
	return packRecallContextWithIntentTraceAndAnchorCap(intent, ordered, pool, cap, cap)
}

func packRecallContextWithIntentTraceAndAnchorCap(intent domain.QueryIntent, ordered []domain.Hit, pool []domain.Hit, cap int, anchorCap int) ([]domain.Hit, []diagnostic.CandidateSnapshot) {
	input := newContextPackInput(cap)
	if input.cap <= 0 {
		return ordered, nil
	}
	candidatePool := mergeContextPackPool(ordered, pool)
	if len(candidatePool) == 0 {
		return ordered, nil
	}
	candidates := contextPackCandidates(input, candidatePool)
	traceCandidates := append([]contextPackCandidate(nil), candidates...)
	dropReasons := map[string]string{}
	candidates = contextPackLimitObservationCandidates(candidates, dropReasons)
	selectedCandidates := candidates
	if len(candidates) <= input.cap {
		hits := contextPackHits(selectedCandidates)
		return hits, contextPackTrace(traceCandidates, selectedCandidates, len(ordered), dropReasons)
	}
	anchors := contextPackRankOutputAnchors(candidates, len(ordered), anchorCap)
	selectedCandidates = contextPackFillWithMMR(candidates, anchors, input.cap)
	return contextPackHits(selectedCandidates), contextPackTrace(traceCandidates, selectedCandidates, len(ordered), dropReasons)
}

func contextPackFillWithMMR(candidates, selected []contextPackCandidate, cap int) []contextPackCandidate {
	if cap <= 0 || len(selected) >= cap {
		return selected[:min(len(selected), cap)]
	}
	out := append([]contextPackCandidate(nil), selected...)
	used := make([]bool, len(candidates))
	for i, cand := range candidates {
		if contextPackCandidateDuplicate(cand, out) {
			used[i] = true
		}
	}
	for len(out) < cap {
		best := -1
		bestScore := math.Inf(-1)
		for i, cand := range candidates {
			if used[i] || contextPackCandidateDuplicate(cand, out) {
				continue
			}
			sourcePenalty := contextPackSourceConcentrationPenalty(cand, out)
			adjusted := cand.score -
				contextPackDiversityPenalty(cand, out) -
				sourcePenalty
			if best < 0 || adjusted > bestScore || (math.Abs(adjusted-bestScore) <= 1e-9 && betterContextPackTieBreak(cand, candidates[best])) {
				best = i
				bestScore = adjusted
			}
		}
		if best < 0 {
			break
		}
		used[best] = true
		out = append(out, candidates[best])
	}
	if len(out) < cap {
		for _, cand := range candidates {
			if contextPackCandidateDuplicate(cand, out) {
				continue
			}
			out = append(out, cand)
			if len(out) >= cap {
				break
			}
		}
	}
	return out
}

func mergeContextPackPool(ordered []domain.Hit, pool []domain.Hit) []domain.Hit {
	if len(pool) == 0 {
		out := make([]domain.Hit, len(ordered))
		copy(out, ordered)
		return out
	}
	out := make([]domain.Hit, 0, len(ordered)+len(pool))
	out = append(out, ordered...)
	out = append(out, pool...)
	return out
}

func contextPackCandidates(input contextPackInput, hits []domain.Hit) []contextPackCandidate {
	maxScore := 0.0
	for _, hit := range hits {
		if hit.Score > maxScore {
			maxScore = hit.Score
		}
	}
	out := make([]contextPackCandidate, 0, len(hits))
	seenFacts := map[string]struct{}{}
	for i, hit := range hits {
		evidenceKey := primaryEvidenceKey(hit)
		candidate := newContextPackCandidate(hit, i, maxScore, evidenceKey)
		if hit.Fact.ID != "" {
			if _, ok := seenFacts[hit.Fact.ID]; ok {
				continue
			}
		}
		if contextPackCandidateDuplicate(candidate, out) {
			continue
		}
		if hit.Fact.ID != "" {
			seenFacts[hit.Fact.ID] = struct{}{}
		}
		out = append(out, candidate)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if math.Abs(out[i].score-out[j].score) > 1e-9 {
			return out[i].score > out[j].score
		}
		return betterContextPackTieBreak(out[i], out[j])
	})
	return out
}

func contextPackLimitObservationCandidates(candidates []contextPackCandidate, dropReasons ...map[string]string) []contextPackCandidate {
	if len(candidates) == 0 {
		return candidates
	}
	factCount := 0
	observationCount := 0
	for _, cand := range candidates {
		if contextPackObservationCandidate(cand) {
			observationCount++
			continue
		}
		factCount++
	}
	if observationCount == 0 {
		return candidates
	}
	cap := contextPackObservationDefaultCap
	if factCount == 0 {
		cap = contextPackObservationOnlyCap
	}
	out := make([]contextPackCandidate, 0, len(candidates))
	keptObservation := 0
	for _, cand := range candidates {
		if !contextPackObservationCandidate(cand) {
			out = append(out, cand)
			continue
		}
		if keptObservation >= cap {
			contextPackRecordDropReason(dropReasons, cand, "observation_lane_cap")
			continue
		}
		out = append(out, cand)
		keptObservation++
	}
	if keptObservation == 0 && factCount == 0 {
		for _, cand := range candidates {
			if !contextPackObservationCandidate(cand) {
				continue
			}
			out = append(out, cand)
			keptObservation++
			if keptObservation >= cap {
				break
			}
		}
	}
	return out
}

func contextPackRecordDropReason(dropReasons []map[string]string, cand contextPackCandidate, reason string) {
	if len(dropReasons) == 0 || dropReasons[0] == nil {
		return
	}
	dropReasons[0][contextPackCandidateTraceKey(cand)] = reason
}

func contextPackRankOutputAnchors(candidates []contextPackCandidate, rankOutputCount, anchorCap int) []contextPackCandidate {
	if anchorCap <= 0 || rankOutputCount <= 0 {
		return nil
	}
	byRank := append([]contextPackCandidate(nil), candidates...)
	sort.SliceStable(byRank, func(i, j int) bool {
		return byRank[i].queryRank < byRank[j].queryRank
	})
	out := make([]contextPackCandidate, 0, anchorCap)
	for _, cand := range byRank {
		if cand.queryRank >= rankOutputCount || contextPackObservationCandidate(cand) {
			continue
		}
		if contextPackCandidateDuplicate(cand, out) {
			continue
		}
		out = append(out, cand)
		if len(out) >= anchorCap {
			break
		}
	}
	return out
}

func contextPackRankOutputAnchorCount(cap int) int {
	if cap <= 0 {
		return 0
	}
	count := (cap + contextPackRankOutputAnchorDivisor - 1) / contextPackRankOutputAnchorDivisor
	if count < 1 {
		return 1
	}
	if count > cap {
		return cap
	}
	return count
}

func contextPackObservationCandidate(cand contextPackCandidate) bool {
	hit := cand.hit
	if hit.Ref.Kind == domain.GraphNodeObservation || hit.Observation.ID != "" {
		return true
	}
	for _, source := range hit.Sources {
		if source == "observation" {
			return true
		}
	}
	return false
}

func newContextPackInput(cap int) contextPackInput {
	return contextPackInput{cap: cap}
}

func newContextPackCandidate(hit domain.Hit, queryRank int, maxHitScore float64, evidenceKey string) contextPackCandidate {
	candidate := contextPackCandidate{
		hit:           hit,
		baseScore:     hit.Score,
		queryRank:     queryRank,
		evidenceKey:   evidenceKey,
		evidenceGroup: primaryEvidenceGroup(hit),
	}
	candidate.score = contextPackScore(candidate, maxHitScore)
	return candidate
}

func contextPackScore(candidate contextPackCandidate, maxHitScore float64) float64 {
	base := 0.0
	if maxHitScore > 0 && candidate.baseScore > 0 {
		base = candidate.baseScore / maxHitScore
		if base > 1 {
			base = 1
		}
	}
	rankPrior := 1 / (1 + float64(candidate.queryRank)/30)
	score := contextPackBaseScoreWeight*base +
		contextPackRankPriorWeight*rankPrior
	if contextPackObservationCandidate(candidate) {
		// Observation hits are raw evidence support. Keep them from outranking
		// structured hits when both have passed assessment.
		score = 0.04*base + 0.08*rankPrior
	}
	return score
}

func betterContextPackTieBreak(a, b contextPackCandidate) bool {
	if a.baseScore != b.baseScore {
		return a.baseScore > b.baseScore
	}
	return a.queryRank < b.queryRank
}

func contextPackDiversityPenalty(candidate contextPackCandidate, selected []contextPackCandidate) float64 {
	if candidate.evidenceGroup == "" {
		return 0
	}
	maxSimilarity := 0.0
	for _, existing := range selected {
		similarity := 0.0
		if candidate.evidenceGroup == existing.evidenceGroup {
			similarity = 0.45
		}
		if similarity > maxSimilarity {
			maxSimilarity = similarity
		}
	}
	if maxSimilarity <= contextPackDiversityPenaltyFloor {
		return 0
	}
	penalty := contextPackDiversityPenaltyScale * maxSimilarity
	return penalty
}

func contextPackSourceConcentrationPenalty(candidate contextPackCandidate, selected []contextPackCandidate) float64 {
	source := primaryHitSource(candidate.hit)
	if source == "" {
		return 0
	}
	count := 0
	for _, existing := range selected {
		if primaryHitSource(existing.hit) == source {
			count++
		}
	}
	if count < 2 {
		return 0
	}
	penalty := contextPackSourcePenaltyPerExtra * float64(count-1)
	if penalty > contextPackSourcePenaltyMax {
		return contextPackSourcePenaltyMax
	}
	return penalty
}

func primaryHitSource(hit domain.Hit) string {
	if len(hit.Sources) == 0 {
		return ""
	}
	return hit.Sources[0]
}

func contextPackCandidateDuplicate(candidate contextPackCandidate, selected []contextPackCandidate) bool {
	sameEvidenceCount := 0
	for _, existing := range selected {
		if candidate.hit.Fact.ID != "" && candidate.hit.Fact.ID == existing.hit.Fact.ID {
			return true
		}
		if candidate.hit.Observation.ID != "" && candidate.hit.Observation.ID == existing.hit.Observation.ID {
			return true
		}
		if candidate.hit.Link.ID != "" && candidate.hit.Link.ID == existing.hit.Link.ID {
			return true
		}
		if candidate.evidenceKey != "" && candidate.evidenceKey == existing.evidenceKey {
			sameEvidenceCount++
		}
	}
	if candidate.evidenceKey != "" && sameEvidenceCount >= contextPackSameEvidenceSiblingCap {
		return true
	}
	return false
}

func contextPackHits(candidates []contextPackCandidate) []domain.Hit {
	out := make([]domain.Hit, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.hit)
	}
	return out
}

func contextPackTrace(candidates, selected []contextPackCandidate, rankOutputCount int, dropReasonMaps ...map[string]string) []diagnostic.CandidateSnapshot {
	if len(candidates) == 0 {
		return nil
	}
	var explicitDropReasons map[string]string
	if len(dropReasonMaps) > 0 {
		explicitDropReasons = dropReasonMaps[0]
	}
	out := make([]diagnostic.CandidateSnapshot, 0, len(candidates))
	for _, cand := range candidates {
		contextPackRank := contextPackSelectedRank(cand, selected)
		rankOutputRank := 0
		if cand.queryRank < rankOutputCount {
			rankOutputRank = cand.queryRank + 1
		}
		droppedReason := ""
		if contextPackRank == 0 {
			droppedReason = "capacity"
			if explicitDropReasons != nil {
				if reason := explicitDropReasons[contextPackCandidateTraceKey(cand)]; reason != "" {
					droppedReason = reason
				}
			}
		}
		routes := append([]string(nil), cand.hit.Sources...)
		out = append(out, diagnostic.CandidateSnapshot{
			FactID:           contextPackTraceFactID(cand.hit),
			Source:           primaryHitSource(cand.hit),
			Rank:             cand.queryRank + 1,
			ScoreLabel:       scoreLabelContextPackRank,
			RankScore:        cand.baseScore,
			EvidenceIDs:      contextPackTraceEvidenceIDs(cand.hit),
			Sources:          routes,
			RankOutputRank:   rankOutputRank,
			ContextPackRank:  contextPackRank,
			PrimarySource:    primaryHitSource(cand.hit),
			ProjectionRoutes: routes,
			DroppedReason:    droppedReason,
		})
	}
	return out
}

func contextPackCandidateTraceKey(cand contextPackCandidate) string {
	if id := contextPackTraceFactID(cand.hit); id != "" {
		return "id:" + id
	}
	if cand.evidenceKey != "" {
		return "ev:" + cand.evidenceKey
	}
	return "rank:" + strconv.Itoa(cand.queryRank)
}

func contextPackSelectedRank(candidate contextPackCandidate, selected []contextPackCandidate) int {
	for i, existing := range selected {
		if sameContextPackCandidate(candidate, existing) {
			return i + 1
		}
	}
	return 0
}

func sameContextPackCandidate(a, b contextPackCandidate) bool {
	if a.hit.Fact.ID != "" && a.hit.Fact.ID == b.hit.Fact.ID {
		return true
	}
	if a.hit.Observation.ID != "" && a.hit.Observation.ID == b.hit.Observation.ID {
		return true
	}
	return false
}

func contextPackTraceFactID(hit domain.Hit) string {
	if hit.Fact.ID != "" {
		return hit.Fact.ID
	}
	if hit.Observation.ID != "" {
		return hit.Observation.ID
	}
	if hit.Link.ID != "" {
		return hit.Link.ID
	}
	return hit.Ref.ID
}

func contextPackTraceEvidenceIDs(hit domain.Hit) []string {
	seen := map[string]struct{}{}
	var out []string
	appendID := func(id string) {
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
	for _, ref := range hit.Evidence {
		appendID(ref.ID)
		appendID(ref.ObservationID)
		appendID(ref.SpanID)
	}
	for _, ref := range hit.Fact.EvidenceRefs {
		appendID(ref.ID)
		appendID(ref.ObservationID)
		appendID(ref.SpanID)
	}
	if hit.Observation.ID != "" {
		appendID(hit.Observation.ID)
		for _, span := range hit.Observation.Spans {
			appendID(span.ID)
		}
	}
	return out
}

func primaryEvidenceKey(hit domain.Hit) string {
	evidence := hit.Evidence
	if len(evidence) == 0 {
		evidence = hit.Fact.EvidenceRefs
	}
	for _, ref := range evidence {
		if ref.ID != "" {
			return "id:" + ref.ID
		}
		if ref.MessageID != "" {
			return "msg:" + ref.MessageID
		}
	}
	return ""
}

func primaryEvidenceGroup(hit domain.Hit) string {
	evidence := hit.Evidence
	if len(evidence) == 0 {
		evidence = hit.Fact.EvidenceRefs
	}
	for _, ref := range evidence {
		if group := evidenceRefSourceGroup(ref); group != "" {
			return group
		}
	}
	return ""
}

func evidenceRefSourceGroup(ref domain.EvidenceRef) string {
	if observationID := strings.TrimSpace(ref.ObservationID); observationID != "" {
		return "obs:" + observationID
	}
	if messageID := strings.TrimSpace(ref.MessageID); messageID != "" {
		return "msg:" + messageID
	}
	return ""
}
