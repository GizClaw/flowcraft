package stages

import (
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	recallintent "github.com/GizClaw/flowcraft/memory/recall/internal/intent"
)

const (
	contextPackDuplicateJaccardCutoff = 0.86
	contextPackQueryFloor             = 0.20

	contextPackEvidenceTextWeight = 0.65
	contextPackFactTextWeight     = 0.35

	contextPackBaseScoreWeight   = 0.22
	contextPackTextScoreWeight   = 0.58
	contextPackRankPriorWeight   = 0.15
	contextPackSignalScoreWeight = 0.05

	contextPackTextTokenCoverageWeight = 0.70
	contextPackTextNumericBoost        = 0.20
	contextPackTextNumericIntentBoost  = 0.10
	contextPackTextTimeBoost           = 0.14
	contextPackTextQuotedBoost         = 0.12
	contextPackTextProperBoost         = 0.10

	contextPackTimeSignalBoost    = 0.35
	contextPackNumericSignalBoost = 0.30
	contextPackQuotedSignalBoost  = 0.20
	contextPackProperSignalBoost  = 0.15

	contextPackSourcePenaltyPerExtra = 0.03
	contextPackSourcePenaltyMax      = 0.12
	contextPackDiversityPenaltyScale = 0.22
	contextPackDiversityPenaltyFloor = 0.35
	contextPackHighTextScoreFloor    = 0.55
	contextPackRankedHeadTextFloor   = 0.18
	contextPackSiblingTextFloor      = 0.16
	contextPackCompletenessTextFloor = 0.08
)

type contextPackInput struct {
	features contextPackQueryFeatures
	now      time.Time
	cap      int
}

type contextPackQueryFeatures struct {
	tokens           map[string]struct{}
	numeric          map[string]struct{}
	quoted           map[string]struct{}
	proper           map[string]struct{}
	collectionIntent bool
	bridgeIntent     bool
	hasTimeSignal    bool
	hasNumericIntent bool
}

type contextPackCandidate struct {
	hit            domain.Hit
	score          float64
	baseScore      float64
	textScore      float64
	queryRank      int
	evidenceKey    string
	evidenceGroup  string
	evidenceTokens map[string]struct{}
	factTokens     map[string]struct{}
	hasTimeSignal  bool
	hasNumeric     bool
}

// packRecallContextWithFeatures fits the read pipeline's ranked evidence into
// the final context budget. It deliberately stays answer-agnostic: it keeps
// diverse, grounded, query-relevant memories visible, while the QA/eval layer
// decides which evidence answers a specific question.
func packRecallContextWithFeatures(query string, features domain.QueryFeatures, now time.Time, ordered []domain.Hit, pool []domain.Hit, cap int) []domain.Hit {
	input := newContextPackInput(query, features, now, cap)
	if input.cap <= 0 {
		return ordered
	}
	candidatePool := mergeContextPackPool(ordered, pool)
	if len(candidatePool) == 0 {
		return ordered
	}
	candidates := contextPackCandidates(input, candidatePool)
	if len(candidates) <= input.cap {
		return contextPackHits(candidates)
	}
	selected := make([]domain.Hit, 0, input.cap)
	selectedCandidates := make([]contextPackCandidate, 0, input.cap)
	used := make([]bool, len(candidates))
	for len(selected) < input.cap {
		best := -1
		bestScore := math.Inf(-1)
		for i, cand := range candidates {
			if used[i] || contextPackCandidateDuplicate(cand, selectedCandidates) {
				continue
			}
			sourcePenalty := 0.0
			if !input.features.collectionIntent {
				sourcePenalty = contextPackSourceConcentrationPenalty(cand, selectedCandidates)
			}
			adjusted := cand.score -
				contextPackDiversityPenalty(cand, selectedCandidates) -
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
		selected = append(selected, candidates[best].hit)
		selectedCandidates = append(selectedCandidates, candidates[best])
	}
	if len(selected) < input.cap {
		for _, cand := range candidates {
			if contextPackCandidateDuplicate(cand, selectedCandidates) {
				continue
			}
			selected = append(selected, cand.hit)
			selectedCandidates = append(selectedCandidates, cand)
			if len(selected) >= input.cap {
				break
			}
		}
	}
	selected, selectedCandidates = contextPackEnsureSignalCoverage(input.features, candidates, selected, selectedCandidates)
	selected, selectedCandidates = contextPackEnsureCollectionCoverage(input.features, candidates, selected, selectedCandidates, input.cap)
	selected, selectedCandidates = contextPackEnsureBridgeCoverage(input.features, candidates, selected, selectedCandidates, input.cap)
	selected, selectedCandidates = contextPackEnsureGroupCompleteness(input.features, candidates, selected, selectedCandidates, input.cap)
	selected, selectedCandidates = contextPackEnsureRankedHeadCoverage(candidates, selected, selectedCandidates, input.cap)
	_ = selectedCandidates
	return selected
}

func packRecallContext(query string, ordered []domain.Hit, pool []domain.Hit, cap int) []domain.Hit {
	return packRecallContextWithFeatures(query, recallintent.ExtractFeatures(query), time.Now(), ordered, pool, cap)
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
		if hit.Fact.ID != "" {
			if _, ok := seenFacts[hit.Fact.ID]; ok {
				continue
			}
			seenFacts[hit.Fact.ID] = struct{}{}
		}
		evidenceKey := primaryEvidenceKey(hit)
		candidate := newContextPackCandidate(input, hit, i, maxScore, evidenceKey)
		if evidenceKey != "" {
			if existing, ok := seenEvidence[evidenceKey]; ok {
				if betterContextPackRepresentative(candidate, out[existing]) {
					out[existing] = candidate
				}
				continue
			}
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

func newContextPackInput(query string, features domain.QueryFeatures, now time.Time, cap int) contextPackInput {
	if now.IsZero() {
		now = time.Now()
	}
	return contextPackInput{
		features: newContextPackQueryFeatures(query, features),
		now:      now,
		cap:      cap,
	}
}

func newContextPackQueryFeatures(query string, features domain.QueryFeatures) contextPackQueryFeatures {
	return contextPackQueryFeatures{
		tokens:           features.Tokens,
		numeric:          features.Numeric,
		quoted:           features.Quoted,
		proper:           features.Proper,
		collectionIntent: contextPackCollectionIntent(query, features),
		bridgeIntent:     contextPackBridgeIntent(query, features),
		hasTimeSignal:    features.HasTimeSignal(),
		hasNumericIntent: features.NumericIntent,
	}
}

func contextPackCollectionIntent(query string, features domain.QueryFeatures) bool {
	if hasNumericIntentKind(features.NumericIntentKind, domain.QueryNumericIntentCount) ||
		hasNumericIntentKind(features.NumericIntentKind, domain.QueryNumericIntentFrequency) {
		return true
	}
	text := strings.ToLower(query)
	if strings.Contains(text, "how many") || strings.Contains(text, "how much") {
		return true
	}
	if strings.Contains(text, "what ") || strings.Contains(text, "which ") {
		if tokenSetHasAny(features.Tokens,
			"item", "thing", "event", "activ", "activity", "kind", "type", "style",
			"name", "person", "people", "place", "medium", "media", "artist",
			"band", "book", "movie", "song", "sport", "country", "food", "breed", "pet") {
			return true
		}
		return strings.Contains(text, " has ") || strings.Contains(text, " have ")
	}
	return false
}

func contextPackBridgeIntent(query string, features domain.QueryFeatures) bool {
	if len(features.Proper) >= 2 {
		return true
	}
	text := strings.ToLower(query)
	return strings.Contains(text, " that ") ||
		strings.Contains(text, " which ") ||
		strings.Contains(text, " who ") ||
		strings.Contains(text, " after ") ||
		strings.Contains(text, " before ") ||
		strings.Contains(text, " because ") ||
		strings.Contains(text, " her ") ||
		strings.Contains(text, " his ") ||
		strings.Contains(text, " their ")
}

func hasNumericIntentKind(kinds []domain.QueryNumericIntentKind, want domain.QueryNumericIntentKind) bool {
	return slices.Contains(kinds, want)
}

func newContextPackCandidate(input contextPackInput, hit domain.Hit, queryRank int, maxHitScore float64, evidenceKey string) contextPackCandidate {
	evidenceText := hitEvidenceText(hit)
	factText := contextPackFactText(hit)
	evidenceTokens := recallintent.TextTokenSet(evidenceText)
	factTokens := recallintent.TextTokenSet(factText)
	hasNumeric := len(recallintent.NumericTokens(evidenceText)) > 0 || len(recallintent.NumericTokens(factText)) > 0
	hasTime := false
	if input.features.hasTimeSignal {
		hasTime = hitTimeSignal(hit, evidenceText+" "+factText, input.now)
	}
	evidenceScore := contextPackTextScore(input.features, evidenceTokens, recallintent.NumericTokens(evidenceText), recallintent.QuotedTokenSet(evidenceText), recallintent.ProperNounSet(evidenceText), hasTime, hasNumeric)
	factScore := contextPackTextScore(input.features, factTokens, recallintent.NumericTokens(factText), recallintent.QuotedTokenSet(factText), recallintent.ProperNounSet(factText), hasTime, hasNumeric)
	textScore := contextPackEvidenceTextWeight*evidenceScore + contextPackFactTextWeight*factScore
	candidate := contextPackCandidate{
		hit:            hit,
		baseScore:      hit.Score,
		textScore:      textScore,
		queryRank:      queryRank,
		evidenceKey:    evidenceKey,
		evidenceGroup:  primaryEvidenceGroup(hit),
		evidenceTokens: evidenceTokens,
		factTokens:     factTokens,
		hasTimeSignal:  hasTime,
		hasNumeric:     hasNumeric,
	}
	candidate.score = contextPackScore(input.features, candidate, maxHitScore)
	return candidate
}

func contextPackTextScore(query contextPackQueryFeatures, textTokens, textNumeric, textQuoted, textProper map[string]struct{}, hasTimeSignal, hasNumeric bool) float64 {
	score := 0.0
	if len(query.tokens) > 0 && len(textTokens) > 0 {
		matched := 0
		for tok := range query.tokens {
			if _, ok := textTokens[tok]; ok {
				matched++
			}
		}
		score += contextPackTextTokenCoverageWeight * float64(matched) / float64(len(query.tokens))
	}
	if intersects(query.numeric, textNumeric) {
		score += contextPackTextNumericBoost
	}
	if query.hasNumericIntent && hasNumeric {
		score += contextPackTextNumericIntentBoost
	}
	if query.hasTimeSignal && hasTimeSignal {
		score += contextPackTextTimeBoost
	}
	if len(query.quoted) > 0 && (intersects(query.quoted, textQuoted) || intersects(query.quoted, textTokens)) {
		score += contextPackTextQuotedBoost
	}
	if intersects(query.proper, textProper) || intersects(query.proper, textTokens) {
		score += contextPackTextProperBoost
	}
	if score > 1 {
		return 1
	}
	return score
}

func contextPackScore(query contextPackQueryFeatures, candidate contextPackCandidate, maxHitScore float64) float64 {
	base := 0.0
	if maxHitScore > 0 && candidate.baseScore > 0 {
		base = candidate.baseScore / maxHitScore
		if base > 1 {
			base = 1
		}
	}
	rankPrior := 1 / (1 + float64(candidate.queryRank)/30)
	signalPresence := 0.0
	if query.hasTimeSignal && candidate.hasTimeSignal {
		signalPresence += contextPackTimeSignalBoost
	}
	if query.hasNumericIntent && candidate.hasNumeric {
		signalPresence += contextPackNumericSignalBoost
	}
	if len(query.quoted) > 0 && (intersects(query.quoted, candidate.evidenceTokens) || intersects(query.quoted, candidate.factTokens)) {
		signalPresence += contextPackQuotedSignalBoost
	}
	if len(query.proper) > 0 && (intersects(query.proper, candidate.evidenceTokens) || intersects(query.proper, candidate.factTokens)) {
		signalPresence += contextPackProperSignalBoost
	}
	if signalPresence > 1 {
		signalPresence = 1
	}
	return contextPackBaseScoreWeight*base +
		contextPackTextScoreWeight*candidate.textScore +
		contextPackRankPriorWeight*rankPrior +
		contextPackSignalScoreWeight*signalPresence
}

func contextPackEnsureSignalCoverage(query contextPackQueryFeatures, candidates []contextPackCandidate, selected []domain.Hit, selectedCandidates []contextPackCandidate) ([]domain.Hit, []contextPackCandidate) {
	if len(selected) == 0 || len(candidates) == 0 {
		return selected, selectedCandidates
	}
	out := append([]domain.Hit(nil), selected...)
	outCandidates := append([]contextPackCandidate(nil), selectedCandidates...)
	needs := []func(contextPackCandidate) bool{}
	if len(query.tokens) > 0 && contextPackQueryCoverage(query, outCandidates) < 0.55 {
		covered := contextPackCoveredQueryTokens(query, outCandidates)
		needs = append(needs, func(c contextPackCandidate) bool {
			return contextPackCoverageGain(query, c, covered) >= 0.18
		})
	}
	if query.hasTimeSignal && !contextPackAnySelected(outCandidates, func(c contextPackCandidate) bool { return c.hasTimeSignal }) {
		needs = append(needs, func(c contextPackCandidate) bool { return c.hasTimeSignal && c.textScore >= 0.18 })
	}
	if query.hasNumericIntent && !contextPackAnySelected(outCandidates, func(c contextPackCandidate) bool { return c.hasNumeric }) {
		needs = append(needs, func(c contextPackCandidate) bool { return c.hasNumeric && c.textScore >= 0.18 })
	}
	if len(query.quoted) > 0 && !contextPackAnySelected(outCandidates, func(c contextPackCandidate) bool {
		return intersects(query.quoted, c.evidenceTokens) || intersects(query.quoted, c.factTokens)
	}) {
		needs = append(needs, func(c contextPackCandidate) bool {
			return intersects(query.quoted, c.evidenceTokens) || intersects(query.quoted, c.factTokens)
		})
	}
	if len(query.proper) > 0 && !contextPackAnySelected(outCandidates, func(c contextPackCandidate) bool {
		return intersects(query.proper, c.evidenceTokens) || intersects(query.proper, c.factTokens)
	}) {
		needs = append(needs, func(c contextPackCandidate) bool {
			return intersects(query.proper, c.evidenceTokens) || intersects(query.proper, c.factTokens)
		})
	}
	for _, need := range needs {
		best := -1
		for i, cand := range candidates {
			if !need(cand) || contextPackCandidateDuplicate(cand, outCandidates) {
				continue
			}
			if best < 0 || cand.score > candidates[best].score {
				best = i
			}
		}
		if best < 0 {
			continue
		}
		replace := contextPackLowestUtilityReplacement(outCandidates, candidates[best])
		if replace < 0 {
			continue
		}
		out[replace] = candidates[best].hit
		outCandidates[replace] = candidates[best]
	}
	return out, outCandidates
}

func contextPackEnsureCollectionCoverage(query contextPackQueryFeatures, candidates []contextPackCandidate, selected []domain.Hit, selectedCandidates []contextPackCandidate, cap int) ([]domain.Hit, []contextPackCandidate) {
	if !query.collectionIntent || cap <= 1 || len(selected) == 0 || len(candidates) == 0 {
		return selected, selectedCandidates
	}
	out := append([]domain.Hit(nil), selected...)
	outCandidates := append([]contextPackCandidate(nil), selectedCandidates...)
	targetAdds := min(6, max(2, cap/5))
	added := 0
	for _, cand := range candidates {
		if added >= targetAdds {
			break
		}
		if cand.textScore < contextPackSiblingTextFloor || contextPackCandidateDuplicate(cand, outCandidates) {
			continue
		}
		if !contextPackCollectionCandidateUseful(query, cand, outCandidates) {
			continue
		}
		replace := contextPackCollectionReplacement(outCandidates, cand)
		if replace < 0 {
			continue
		}
		out[replace] = cand.hit
		outCandidates[replace] = cand
		added++
	}
	return out, outCandidates
}

func contextPackCollectionCandidateUseful(query contextPackQueryFeatures, cand contextPackCandidate, selected []contextPackCandidate) bool {
	if cand.textScore >= contextPackQueryFloor {
		return true
	}
	for _, existing := range selected {
		if cand.evidenceGroup != "" && cand.evidenceGroup == existing.evidenceGroup {
			return true
		}
		if collectionSiblingFacts(cand.hit.Fact, existing.hit.Fact) && contextPackCoverageGain(query, cand, contextPackCoveredQueryTokens(query, selected)) > 0 {
			return true
		}
	}
	return false
}

func contextPackCollectionReplacement(selected []contextPackCandidate, incoming contextPackCandidate) int {
	replace := -1
	for i, cand := range selected {
		if cand.queryRank < 5 && cand.textScore >= contextPackHighTextScoreFloor {
			continue
		}
		if cand.textScore >= incoming.textScore && cand.score >= incoming.score {
			continue
		}
		if replace < 0 || contextPackUtility(cand) < contextPackUtility(selected[replace]) {
			replace = i
		}
	}
	return replace
}

func contextPackEnsureBridgeCoverage(query contextPackQueryFeatures, candidates []contextPackCandidate, selected []domain.Hit, selectedCandidates []contextPackCandidate, cap int) ([]domain.Hit, []contextPackCandidate) {
	if !query.bridgeIntent || cap <= 1 || len(selected) == 0 || len(candidates) == 0 || contextPackQueryCoverage(query, selectedCandidates) >= 0.85 {
		return selected, selectedCandidates
	}
	out := append([]domain.Hit(nil), selected...)
	outCandidates := append([]contextPackCandidate(nil), selectedCandidates...)
	covered := contextPackCoveredQueryTokens(query, outCandidates)
	added := 0
	for _, cand := range candidates {
		if added >= 2 {
			break
		}
		if cand.textScore < contextPackSiblingTextFloor || contextPackCandidateDuplicate(cand, outCandidates) {
			continue
		}
		if !contextPackAssociatedCandidate(cand, outCandidates) || contextPackCoverageGain(query, cand, covered) <= 0 {
			continue
		}
		replace := contextPackLowestUtilityReplacement(outCandidates, cand)
		if replace < 0 {
			continue
		}
		out[replace] = cand.hit
		outCandidates[replace] = cand
		covered = contextPackCoveredQueryTokens(query, outCandidates)
		added++
	}
	return out, outCandidates
}

func contextPackAssociatedCandidate(cand contextPackCandidate, selected []contextPackCandidate) bool {
	for _, existing := range selected {
		if cand.evidenceGroup != "" && cand.evidenceGroup == existing.evidenceGroup {
			return true
		}
		if collectionSiblingFacts(cand.hit.Fact, existing.hit.Fact) {
			return true
		}
	}
	return false
}

func contextPackEnsureGroupCompleteness(query contextPackQueryFeatures, candidates []contextPackCandidate, selected []domain.Hit, selectedCandidates []contextPackCandidate, cap int) ([]domain.Hit, []contextPackCandidate) {
	if !contextPackCompletenessIntent(query) || cap <= 1 || len(selected) == 0 || len(candidates) == 0 {
		return selected, selectedCandidates
	}
	out := append([]domain.Hit(nil), selected...)
	outCandidates := append([]contextPackCandidate(nil), selectedCandidates...)
	targetAdds := min(4, max(1, cap/8))
	if query.collectionIntent {
		targetAdds = min(6, max(targetAdds, max(2, cap/6)))
	}
	added := 0
	for _, cand := range candidates {
		if added >= targetAdds {
			break
		}
		if cand.textScore < contextPackCompletenessTextFloor || contextPackCandidateDuplicate(cand, outCandidates) {
			continue
		}
		if !contextPackCompletenessCandidateUseful(query, cand, outCandidates) {
			continue
		}
		if len(out) < cap {
			out = append(out, cand.hit)
			outCandidates = append(outCandidates, cand)
			added++
			continue
		}
		replace := contextPackCompletenessReplacement(query, outCandidates, cand)
		if replace < 0 {
			continue
		}
		out[replace] = cand.hit
		outCandidates[replace] = cand
		added++
	}
	return out, outCandidates
}

func contextPackCompletenessIntent(query contextPackQueryFeatures) bool {
	return query.collectionIntent || query.bridgeIntent || query.hasTimeSignal || query.hasNumericIntent
}

func contextPackCompletenessCandidateUseful(query contextPackQueryFeatures, cand contextPackCandidate, selected []contextPackCandidate) bool {
	covered := contextPackCoveredQueryTokens(query, selected)
	if cand.textScore >= contextPackSiblingTextFloor && contextPackCoverageGain(query, cand, covered) > 0 {
		return true
	}
	for _, existing := range selected {
		if contextPackStrongSibling(query, cand, existing) {
			return true
		}
	}
	return false
}

func contextPackStrongSibling(query contextPackQueryFeatures, a, b contextPackCandidate) bool {
	if query.collectionIntent && sameSubjectPredicate(a.hit.Fact, b.hit.Fact) {
		return true
	}
	if query.collectionIntent && collectionSiblingFacts(a.hit.Fact, b.hit.Fact) && contextPackSharesQueryAnchor(query, a, b) {
		return true
	}
	if query.bridgeIntent {
		if a.evidenceGroup != "" && a.evidenceGroup == b.evidenceGroup {
			return true
		}
		if collectionSiblingFacts(a.hit.Fact, b.hit.Fact) && contextPackSharesQueryAnchor(query, a, b) {
			return true
		}
	}
	if query.hasTimeSignal && a.hasTimeSignal && contextPackTemporalSibling(a.hit.Fact, b.hit.Fact) {
		return true
	}
	if query.hasNumericIntent && a.hasNumeric && collectionSiblingFacts(a.hit.Fact, b.hit.Fact) {
		return true
	}
	return false
}

func contextPackSharesQueryAnchor(query contextPackQueryFeatures, a, b contextPackCandidate) bool {
	if intersects(query.proper, a.evidenceTokens) || intersects(query.proper, a.factTokens) ||
		intersects(query.proper, b.evidenceTokens) || intersects(query.proper, b.factTokens) {
		return true
	}
	if intersects(query.quoted, a.evidenceTokens) || intersects(query.quoted, a.factTokens) ||
		intersects(query.quoted, b.evidenceTokens) || intersects(query.quoted, b.factTokens) {
		return true
	}
	return intersects(query.tokens, a.factTokens) && intersects(query.tokens, b.factTokens)
}

func contextPackTemporalSibling(a, b domain.TemporalFact) bool {
	if sameSubjectPredicate(a, b) {
		return true
	}
	if strings.TrimSpace(a.Subject) != "" && strings.TrimSpace(b.Subject) != "" && strings.EqualFold(a.Subject, b.Subject) {
		return true
	}
	if a.ValidFrom != nil && b.ValidFrom != nil && sameDay(*a.ValidFrom, *b.ValidFrom) {
		return true
	}
	return !a.ObservedAt.IsZero() && !b.ObservedAt.IsZero() && sameDay(a.ObservedAt, b.ObservedAt)
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

func contextPackCompletenessReplacement(query contextPackQueryFeatures, selected []contextPackCandidate, incoming contextPackCandidate) int {
	replace := -1
	bestUtility := math.Inf(1)
	for i, cand := range selected {
		if cand.queryRank < 5 && cand.textScore >= contextPackHighTextScoreFloor {
			continue
		}
		if contextPackStrongSibling(query, cand, incoming) && cand.textScore >= contextPackCompletenessTextFloor {
			continue
		}
		utility := contextPackUtility(cand)
		if contextPackCompletenessCandidateUseful(query, cand, selectedWithoutIndex(selected, i)) {
			utility += 0.20
		}
		if utility < bestUtility {
			bestUtility = utility
			replace = i
		}
	}
	return replace
}

func selectedWithoutIndex(selected []contextPackCandidate, idx int) []contextPackCandidate {
	if idx < 0 || idx >= len(selected) || len(selected) == 0 {
		return selected
	}
	out := make([]contextPackCandidate, 0, len(selected)-1)
	out = append(out, selected[:idx]...)
	out = append(out, selected[idx+1:]...)
	return out
}

func contextPackEnsureRankedHeadCoverage(candidates []contextPackCandidate, selected []domain.Hit, selectedCandidates []contextPackCandidate, cap int) ([]domain.Hit, []contextPackCandidate) {
	if cap <= 0 || len(selected) == 0 || len(candidates) == 0 {
		return selected, selectedCandidates
	}
	out := append([]domain.Hit(nil), selected...)
	outCandidates := append([]contextPackCandidate(nil), selectedCandidates...)
	for _, cand := range candidates {
		if cand.queryRank >= cap || cand.textScore < contextPackRankedHeadTextFloor || contextPackCandidateDuplicate(cand, outCandidates) {
			continue
		}
		replace := contextPackLowestUtilityReplacement(outCandidates, cand)
		if replace < 0 {
			continue
		}
		out[replace] = cand.hit
		outCandidates[replace] = cand
	}
	return out, outCandidates
}

func contextPackQueryCoverage(query contextPackQueryFeatures, selected []contextPackCandidate) float64 {
	if len(query.tokens) == 0 {
		return 1
	}
	return float64(len(contextPackCoveredQueryTokens(query, selected))) / float64(len(query.tokens))
}

func contextPackCoveredQueryTokens(query contextPackQueryFeatures, selected []contextPackCandidate) map[string]struct{} {
	covered := map[string]struct{}{}
	for _, cand := range selected {
		for tok := range query.tokens {
			if _, ok := cand.evidenceTokens[tok]; ok {
				covered[tok] = struct{}{}
				continue
			}
			if _, ok := cand.factTokens[tok]; ok {
				covered[tok] = struct{}{}
			}
		}
	}
	return covered
}

func contextPackCoverageGain(query contextPackQueryFeatures, cand contextPackCandidate, covered map[string]struct{}) float64 {
	if len(query.tokens) == 0 {
		return 0
	}
	newMatches := 0
	for tok := range query.tokens {
		if _, ok := covered[tok]; ok {
			continue
		}
		if _, ok := cand.evidenceTokens[tok]; ok {
			newMatches++
			continue
		}
		if _, ok := cand.factTokens[tok]; ok {
			newMatches++
		}
	}
	return float64(newMatches) / float64(len(query.tokens))
}

func contextPackAnySelected(selected []contextPackCandidate, pred func(contextPackCandidate) bool) bool {
	return slices.ContainsFunc(selected, pred)
}

func contextPackLowestUtilityReplacement(selected []contextPackCandidate, incoming contextPackCandidate) int {
	replace := -1
	for i, cand := range selected {
		if cand.queryRank < 5 && cand.textScore >= contextPackHighTextScoreFloor {
			continue
		}
		if cand.textScore+0.05 >= incoming.textScore && cand.score+0.05 >= incoming.score {
			continue
		}
		if replace < 0 || contextPackUtility(cand) < contextPackUtility(selected[replace]) {
			replace = i
		}
	}
	return replace
}

func contextPackUtility(cand contextPackCandidate) float64 {
	return 0.55*cand.textScore + 0.35*cand.score + 0.10*cand.baseScore
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
	if candidate.textScore >= contextPackHighTextScoreFloor {
		penalty *= 0.5
	}
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
			if candidate.textScore < contextPackQueryFloor {
				return true
			}
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
	if math.Abs(a.textScore-b.textScore) > 1e-9 {
		return a.textScore > b.textScore
	}
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

func collectionSiblingFacts(a, b domain.TemporalFact) bool {
	if a.ID != "" && a.ID == b.ID {
		return false
	}
	if a.Subject != "" && b.Subject != "" && strings.EqualFold(a.Subject, b.Subject) {
		if a.Predicate != "" && b.Predicate != "" && strings.EqualFold(a.Predicate, b.Predicate) {
			return true
		}
		return a.Kind == b.Kind
	}
	if a.Predicate != "" && b.Predicate != "" && strings.EqualFold(a.Predicate, b.Predicate) {
		return a.Kind == b.Kind
	}
	return false
}

func tokenSetHasAny(tokens map[string]struct{}, values ...string) bool {
	for _, value := range values {
		if _, ok := tokens[value]; ok {
			return true
		}
	}
	return false
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

func intersects(a, b map[string]struct{}) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	for tok := range a {
		if _, ok := b[tok]; ok {
			return true
		}
	}
	return false
}

func hitTimeSignal(hit domain.Hit, evidence string, now time.Time) bool {
	if hit.Fact.ValidFrom != nil || hit.Fact.ValidTo != nil {
		return true
	}
	for _, ref := range hit.Evidence {
		if !ref.Timestamp.IsZero() {
			return true
		}
	}
	for _, ref := range hit.Fact.EvidenceRefs {
		if !ref.Timestamp.IsZero() {
			return true
		}
	}
	return recallintent.HasTimex(evidence, now)
}
