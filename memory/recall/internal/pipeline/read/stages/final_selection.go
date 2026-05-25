package stages

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	recallintent "github.com/GizClaw/flowcraft/memory/recall/internal/intent"
)

const (
	duplicateJaccardCutoff  = 0.86
	contextDedupeQueryFloor = 0.20
)

type finalSelectionQueryFeatures struct {
	tokens           map[string]struct{}
	numeric          map[string]struct{}
	quoted           map[string]struct{}
	proper           map[string]struct{}
	hasTimeSignal    bool
	hasNumericIntent bool
	temporalKinds    []domain.QueryTemporalIntentKind
	numericKinds     []domain.QueryNumericIntentKind
}

type finalSelectionCandidate struct {
	hit            domain.Hit
	score          float64
	baseScore      float64
	evidenceScore  float64
	factScore      float64
	queryRank      int
	evidenceKey    string
	evidenceTokens map[string]struct{}
	factTokens     map[string]struct{}
	hasTimeSignal  bool
	hasNumeric     bool
	slotScore      float64
}

func selectFinalEvidenceAwareHitsWithFeatures(features domain.QueryFeatures, ordered []domain.Hit, pool []domain.Hit, cap int) []domain.Hit {
	if cap <= 0 || len(ordered) == 0 {
		return ordered
	}
	return selectFinalHybridRerankHitsWithFeatures(features, ordered, pool, cap)
}

func selectFinalHybridRerankHits(query string, ordered []domain.Hit, pool []domain.Hit, cap int) []domain.Hit {
	return selectFinalHybridRerankHitsWithFeatures(recallintent.ExtractFeatures(query), ordered, pool, cap)
}

func selectFinalHybridRerankHitsWithFeatures(features domain.QueryFeatures, ordered []domain.Hit, pool []domain.Hit, cap int) []domain.Hit {
	candidatePool := mergeFinalSelectionPool(ordered, pool)
	candidates := finalSelectionCandidates(features, candidatePool)
	if len(candidates) <= cap {
		return finalSelectionHits(candidates)
	}
	selected := make([]domain.Hit, 0, cap)
	selectedCandidates := make([]finalSelectionCandidate, 0, cap)
	used := make([]bool, len(candidates))
	for len(selected) < cap {
		best := -1
		bestScore := math.Inf(-1)
		for i, cand := range candidates {
			if used[i] {
				continue
			}
			adjusted := cand.score - finalSelectionDiversityPenalty(cand, selectedCandidates)
			if best < 0 || adjusted > bestScore || (math.Abs(adjusted-bestScore) <= 1e-9 && betterFinalSelectionTieBreak(cand, candidates[best])) {
				best = i
				bestScore = adjusted
			}
		}
		if best < 0 {
			break
		}
		used[best] = true
		if finalSelectionCandidateDuplicate(candidates[best], selectedCandidates) {
			continue
		}
		selected = append(selected, candidates[best].hit)
		selectedCandidates = append(selectedCandidates, candidates[best])
	}
	if len(selected) < cap {
		for _, cand := range candidates {
			if finalSelectionCandidateDuplicate(cand, selectedCandidates) {
				continue
			}
			selected = append(selected, cand.hit)
			selectedCandidates = append(selectedCandidates, cand)
			if len(selected) >= cap {
				break
			}
		}
	}
	selected = finalSelectionRescueAnswerSlot(candidates, selected, selectedCandidates, cap)
	return selected
}

func mergeFinalSelectionPool(ordered []domain.Hit, pool []domain.Hit) []domain.Hit {
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

func finalSelectionCandidates(features domain.QueryFeatures, hits []domain.Hit) []finalSelectionCandidate {
	queryFeatures := newFinalSelectionQueryFeatures(features)
	maxScore := 0.0
	for _, hit := range hits {
		if hit.Score > maxScore {
			maxScore = hit.Score
		}
	}
	out := make([]finalSelectionCandidate, 0, len(hits))
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
		candidate := newFinalSelectionCandidate(queryFeatures, hit, i, maxScore, evidenceKey)
		if evidenceKey != "" {
			if existing, ok := seenEvidence[evidenceKey]; ok {
				if betterFinalSelectionEvidenceRepresentative(candidate, out[existing]) {
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
		return betterFinalSelectionTieBreak(out[i], out[j])
	})
	return out
}

func newFinalSelectionQueryFeatures(features domain.QueryFeatures) finalSelectionQueryFeatures {
	return finalSelectionQueryFeatures{
		tokens:           features.Tokens,
		numeric:          features.Numeric,
		quoted:           features.Quoted,
		proper:           features.Proper,
		hasTimeSignal:    features.HasTimeSignal(),
		hasNumericIntent: features.NumericIntent,
		temporalKinds:    append([]domain.QueryTemporalIntentKind(nil), features.Temporal.IntentKind...),
		numericKinds:     append([]domain.QueryNumericIntentKind(nil), features.NumericIntentKind...),
	}
}

func newFinalSelectionCandidate(queryFeatures finalSelectionQueryFeatures, hit domain.Hit, queryRank int, maxHitScore float64, evidenceKey string) finalSelectionCandidate {
	evidenceText := hitEvidenceText(hit)
	factText := finalSelectionFactText(hit)
	combinedText := factText
	if evidenceText != "" {
		if combinedText != "" {
			combinedText += " "
		}
		combinedText += evidenceText
	}
	evidenceTokens := recallintent.TextTokenSet(evidenceText)
	factTokens := recallintent.TextTokenSet(factText)
	evidenceNumeric := recallintent.NumericTokens(evidenceText)
	factNumeric := recallintent.NumericTokens(factText)
	evidenceQuoted := recallintent.QuotedTokenSet(evidenceText)
	factQuoted := recallintent.QuotedTokenSet(factText)
	evidenceProper := recallintent.ProperNounSet(evidenceText)
	factProper := recallintent.ProperNounSet(factText)
	hasNumeric := len(evidenceNumeric) > 0 || len(factNumeric) > 0
	hasTime := false
	if queryFeatures.hasTimeSignal {
		hasTime = hitHasTimeSignal(hit, combinedText)
	}
	evidenceScore := finalSelectionTextScore(queryFeatures, evidenceTokens, evidenceNumeric, evidenceQuoted, evidenceProper, hasTime, len(evidenceNumeric) > 0)
	factScore := finalSelectionTextScore(queryFeatures, factTokens, factNumeric, factQuoted, factProper, hasTime, len(factNumeric) > 0)
	slotScore := finalSelectionAnswerSlotScore(queryFeatures, hit, combinedText, hasTime, hasNumeric)
	candidate := finalSelectionCandidate{
		hit:            hit,
		baseScore:      hit.Score,
		evidenceScore:  evidenceScore,
		factScore:      factScore,
		queryRank:      queryRank,
		evidenceKey:    evidenceKey,
		evidenceTokens: evidenceTokens,
		factTokens:     factTokens,
		hasTimeSignal:  hasTime,
		hasNumeric:     hasNumeric,
		slotScore:      slotScore,
	}
	candidate.score = finalSelectionScore(queryFeatures, candidate, maxHitScore)
	return candidate
}

func finalSelectionTextScore(query finalSelectionQueryFeatures, textTokens, textNumeric, textQuoted, textProper map[string]struct{}, hasTimeSignal, hasNumeric bool) float64 {
	if len(query.tokens) == 0 || len(textTokens) == 0 {
		return 0
	}
	matched := 0
	for tok := range query.tokens {
		if _, ok := textTokens[tok]; ok {
			matched++
		}
	}
	score := float64(matched) / float64(len(query.tokens))
	if intersects(query.numeric, textNumeric) {
		score += 0.25
	}
	if query.hasNumericIntent && hasNumeric {
		score += 0.15
	}
	if query.hasTimeSignal && hasTimeSignal {
		score += 0.20
	}
	if len(query.quoted) > 0 && (intersects(query.quoted, textQuoted) || intersects(query.quoted, textTokens)) {
		score += 0.15
	}
	if intersects(query.proper, textProper) {
		score += 0.10
	}
	if score > 1 {
		return 1
	}
	return score
}

func finalSelectionScore(query finalSelectionQueryFeatures, candidate finalSelectionCandidate, maxHitScore float64) float64 {
	base := 0.0
	if maxHitScore > 0 && candidate.baseScore > 0 {
		base = candidate.baseScore / maxHitScore
		if base > 1 {
			base = 1
		}
	}
	rankPrior := 1 / (1 + float64(candidate.queryRank)/30)
	score := 0.42*candidate.evidenceScore + 0.35*candidate.factScore + 0.18*base + 0.05*rankPrior
	if candidate.slotScore > 0 {
		score += 0.08 * candidate.slotScore
	}
	if query.hasTimeSignal && candidate.hasTimeSignal {
		score += 0.04
	}
	if query.hasNumericIntent && candidate.hasNumeric {
		score += 0.04
	}
	return score
}

func finalSelectionAnswerSlotScore(query finalSelectionQueryFeatures, hit domain.Hit, text string, hasTime, hasNumeric bool) float64 {
	score := 0.0
	if query.hasTimeSignal {
		if hasTime {
			score += 0.45
		}
		if len(query.temporalKinds) > 0 && hasTime {
			score += 0.05
		}
		if hit.Fact.ValidFrom != nil || hit.Fact.ValidTo != nil {
			score += 0.25
		}
		if recallintent.HasTimex(text, time.Now()) {
			score += 0.15
		}
		if hitHasSource(hit, "timeline") {
			score += 0.10
		}
	}
	if query.hasNumericIntent {
		if hasNumeric {
			score += 0.40
		}
		score += numericKindSlotScore(query.numericKinds, text)
	}
	if score > 1 {
		return 1
	}
	return score
}

func numericKindSlotScore(kinds []domain.QueryNumericIntentKind, text string) float64 {
	if len(kinds) == 0 {
		return 0
	}
	lower := strings.ToLower(text)
	score := 0.0
	for _, kind := range kinds {
		switch kind {
		case domain.QueryNumericIntentPrice:
			if strings.ContainsAny(lower, "$€£") || strings.Contains(lower, "price") || strings.Contains(lower, "cost") {
				score += 0.18
			}
		case domain.QueryNumericIntentPercent:
			if strings.Contains(lower, "%") || strings.Contains(lower, "percent") || strings.Contains(lower, "percentage") {
				score += 0.18
			}
		case domain.QueryNumericIntentFrequency:
			if strings.Contains(lower, "times") || strings.Contains(lower, "often") || strings.Contains(lower, "frequency") {
				score += 0.15
			}
		case domain.QueryNumericIntentDuration:
			if strings.Contains(lower, "day") || strings.Contains(lower, "week") || strings.Contains(lower, "month") || strings.Contains(lower, "year") || strings.Contains(lower, "hour") || strings.Contains(lower, "minute") {
				score += 0.15
			}
		case domain.QueryNumericIntentAge:
			if strings.Contains(lower, "old") || strings.Contains(lower, "age") || strings.Contains(lower, "years") {
				score += 0.12
			}
		default:
			score += 0.08
		}
	}
	if score > 0.35 {
		return 0.35
	}
	return score
}

func hitHasSource(hit domain.Hit, source string) bool {
	for _, s := range hit.Sources {
		if s == source {
			return true
		}
	}
	return false
}

func finalSelectionRescueAnswerSlot(candidates []finalSelectionCandidate, selected []domain.Hit, selectedCandidates []finalSelectionCandidate, cap int) []domain.Hit {
	if cap <= 0 || len(selected) == 0 || len(selected) < cap {
		return selected
	}
	best := -1
	for i, cand := range candidates {
		if cand.slotScore < 0.55 || finalSelectionCandidateDuplicate(cand, selectedCandidates) {
			continue
		}
		if best < 0 || cand.slotScore > candidates[best].slotScore || (cand.slotScore == candidates[best].slotScore && cand.score > candidates[best].score) {
			best = i
		}
	}
	if best < 0 {
		return selected
	}
	replace := -1
	for i, cand := range selectedCandidates {
		if cand.slotScore >= candidates[best].slotScore-0.15 || cand.evidenceScore >= 0.55 {
			continue
		}
		if replace < 0 || cand.score < selectedCandidates[replace].score {
			replace = i
		}
	}
	if replace < 0 {
		return selected
	}
	out := append([]domain.Hit(nil), selected...)
	out[replace] = candidates[best].hit
	return out
}

func betterFinalSelectionTieBreak(a, b finalSelectionCandidate) bool {
	if a.baseScore != b.baseScore {
		return a.baseScore > b.baseScore
	}
	return a.queryRank < b.queryRank
}

func finalSelectionDiversityPenalty(candidate finalSelectionCandidate, selected []finalSelectionCandidate) float64 {
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
	if maxSimilarity <= 0.35 {
		return 0
	}
	penalty := 0.22 * maxSimilarity
	if candidate.evidenceScore >= 0.55 {
		penalty *= 0.5
	}
	return penalty
}

func finalSelectionCandidateDuplicate(candidate finalSelectionCandidate, selected []finalSelectionCandidate) bool {
	for _, existing := range selected {
		if candidate.hit.Fact.ID != "" && candidate.hit.Fact.ID == existing.hit.Fact.ID {
			return true
		}
		if candidate.evidenceKey != "" && candidate.evidenceKey == existing.evidenceKey {
			return true
		}
		if sameStructuredMemory(candidate.hit.Fact, existing.hit.Fact) && tokenSetJaccard(candidate.evidenceTokens, existing.evidenceTokens) >= duplicateJaccardCutoff {
			if candidate.evidenceScore < contextDedupeQueryFloor {
				return true
			}
		}
	}
	return false
}

func finalSelectionHits(candidates []finalSelectionCandidate) []domain.Hit {
	out := make([]domain.Hit, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.hit)
	}
	return out
}

func finalSelectionFactText(hit domain.Hit) string {
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

func betterFinalSelectionEvidenceRepresentative(a, b finalSelectionCandidate) bool {
	aAnswerScore := a.slotScore + 0.55*a.evidenceScore + 0.45*a.factScore
	bAnswerScore := b.slotScore + 0.55*b.evidenceScore + 0.45*b.factScore
	if math.Abs(aAnswerScore-bAnswerScore) > 1e-9 {
		return aAnswerScore > bAnswerScore
	}
	if ak, bk := finalSelectionKindPriority(a.hit.Fact.Kind), finalSelectionKindPriority(b.hit.Fact.Kind); ak != bk {
		return ak > bk
	}
	return betterFinalSelectionTieBreak(a, b)
}

func finalSelectionKindPriority(kind domain.FactKind) int {
	switch kind {
	case domain.KindEvent:
		return 5
	case domain.KindState:
		return 4
	case domain.KindPlan:
		return 3
	case domain.KindPreference, domain.KindProcedure, domain.KindRelation:
		return 2
	case domain.KindNote:
		return 1
	default:
		return 0
	}
}

func hitHasTimeSignal(hit domain.Hit, evidence string) bool {
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
	return recallintent.HasTimex(evidence, time.Now())
}
