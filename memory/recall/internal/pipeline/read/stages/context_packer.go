package stages

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	recallintent "github.com/GizClaw/flowcraft/memory/recall/internal/intent"
)

const (
	contextPackDuplicateJaccardCutoff = 0.86
	contextPackBaseScoreWeight        = 0.85
	contextPackRankPriorWeight        = 0.15

	contextPackSourcePenaltyPerExtra = 0.03
	contextPackSourcePenaltyMax      = 0.12
	contextPackDiversityPenaltyScale = 0.22
	contextPackDiversityPenaltyFloor = 0.35

	contextPackObservationDefaultCap = 1
	contextPackObservationRescueCap  = 4
)

type contextPackInput struct {
	cap int
}

type contextPackCandidate struct {
	hit            domain.Hit
	score          float64
	baseScore      float64
	queryRank      int
	evidenceKey    string
	evidenceGroup  string
	evidenceTokens map[string]struct{}
	factTokens     map[string]struct{}
}

func packRecallContextWithFeaturesAndDetail(ordered []domain.Hit, pool []domain.Hit, cap int) []domain.Hit {
	input := newContextPackInput(cap)
	if input.cap <= 0 {
		return ordered
	}
	candidatePool := mergeContextPackPool(ordered, pool)
	if len(candidatePool) == 0 {
		return ordered
	}
	candidates := contextPackCandidates(input, candidatePool)
	candidates = contextPackLimitObservationCandidates(candidates)
	if len(candidates) <= input.cap {
		return contextPackHits(candidates)
	}
	selectedCandidates := contextPackFillWithMMR(candidates, nil, input.cap)
	return contextPackHits(selectedCandidates)
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
	seenEvidence := map[string]int{}
	for i, hit := range hits {
		evidenceKey := primaryEvidenceKey(hit)
		candidate := newContextPackCandidate(hit, i, maxScore, evidenceKey)
		if evidenceKey != "" {
			if existing, ok := seenEvidence[evidenceKey]; ok {
				if betterContextPackRepresentative(candidate, out[existing]) {
					if oldFactID := out[existing].hit.Fact.ID; oldFactID != "" && oldFactID != candidate.hit.Fact.ID {
						delete(seenFacts, oldFactID)
					}
					out[existing] = candidate
					if candidate.hit.Fact.ID != "" {
						seenFacts[candidate.hit.Fact.ID] = struct{}{}
					}
				}
				continue
			}
		}
		if hit.Fact.ID != "" {
			if _, ok := seenFacts[hit.Fact.ID]; ok {
				continue
			}
			seenFacts[hit.Fact.ID] = struct{}{}
		}
		if evidenceKey != "" {
			seenEvidence[evidenceKey] = len(out)
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

func contextPackLimitObservationCandidates(candidates []contextPackCandidate) []contextPackCandidate {
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
		cap = contextPackObservationRescueCap
	}
	out := make([]contextPackCandidate, 0, len(candidates))
	keptObservation := 0
	for _, cand := range candidates {
		if !contextPackObservationCandidate(cand) {
			out = append(out, cand)
			continue
		}
		if keptObservation >= cap {
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
	evidenceText := hitEvidenceText(hit)
	factText := contextPackFactText(hit)
	evidenceTokens := recallintent.TextTokenSet(evidenceText)
	factTokens := recallintent.TextTokenSet(factText)
	candidate := contextPackCandidate{
		hit:            hit,
		baseScore:      hit.Score,
		queryRank:      queryRank,
		evidenceKey:    evidenceKey,
		evidenceGroup:  primaryEvidenceGroup(hit),
		evidenceTokens: evidenceTokens,
		factTokens:     factTokens,
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
		score *= 0.75
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
	maxSimilarity := 0.0
	for _, existing := range selected {
		similarity := tokenSetJaccard(candidate.evidenceTokens, existing.evidenceTokens)
		if sameStructuredMemory(candidate.hit.Fact, existing.hit.Fact) {
			similarity += 0.15
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
	for _, existing := range selected {
		if candidate.hit.Fact.ID != "" && candidate.hit.Fact.ID == existing.hit.Fact.ID {
			return true
		}
		if candidate.evidenceKey != "" && candidate.evidenceKey == existing.evidenceKey {
			return true
		}
		if sameStructuredMemory(candidate.hit.Fact, existing.hit.Fact) && tokenSetJaccard(candidate.evidenceTokens, existing.evidenceTokens) >= contextPackDuplicateJaccardCutoff {
			return true
		}
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

func betterContextPackRepresentative(a, b contextPackCandidate) bool {
	if math.Abs(a.score-b.score) > 1e-9 {
		return a.score > b.score
	}
	return betterContextPackTieBreak(a, b)
}

func contextPackFactText(hit domain.Hit) string {
	var b strings.Builder
	for _, part := range []string{
		hit.Fact.Content,
		hit.Fact.Subject,
		hit.Fact.Predicate,
		hit.Fact.Object,
		string(hit.Fact.Kind),
		hit.Fact.Location,
	} {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(part)
	}
	for _, entity := range hit.Fact.Entities {
		entity = strings.TrimSpace(entity)
		if entity == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(entity)
	}
	for _, participant := range hit.Fact.Participants {
		participant = strings.TrimSpace(participant)
		if participant == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(participant)
	}
	if b.Len() == 0 {
		return hitEvidenceText(hit)
	}
	return b.String()
}

func hitEvidenceText(hit domain.Hit) string {
	var b strings.Builder
	evidence := hit.Evidence
	if len(evidence) == 0 {
		evidence = hit.Fact.EvidenceRefs
	}
	for _, ref := range evidence {
		if strings.TrimSpace(ref.Text) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(ref.Text)
	}
	if b.Len() == 0 {
		b.WriteString(hit.Fact.EvidenceText)
	}
	return b.String()
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
		for _, raw := range []string{ref.ID, ref.MessageID} {
			if group := evidenceGroup(raw); group != "" {
				return group
			}
		}
	}
	return ""
}

func evidenceGroup(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ';' || r == ',' || r == ' '
	})
	if len(parts) == 0 {
		return ""
	}
	raw = parts[0]
	idx := strings.LastIndex(raw, ":")
	if idx <= 0 || idx == len(raw)-1 {
		return ""
	}
	if _, err := strconv.Atoi(raw[idx+1:]); err != nil {
		return ""
	}
	return raw[:idx]
}

func sameStructuredMemory(a, b domain.TemporalFact) bool {
	if a.Subject != "" && b.Subject != "" && !strings.EqualFold(a.Subject, b.Subject) {
		return false
	}
	if a.Predicate != "" && b.Predicate != "" && !strings.EqualFold(a.Predicate, b.Predicate) {
		return false
	}
	return a.Kind == b.Kind
}

func tokenSetJaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	intersect := 0
	for tok := range a {
		if _, ok := b[tok]; ok {
			intersect++
		}
	}
	union := len(a) + len(b) - intersect
	if union == 0 {
		return 0
	}
	return float64(intersect) / float64(union)
}
