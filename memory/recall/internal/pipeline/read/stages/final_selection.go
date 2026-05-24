package stages

import (
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/text/quotes"
	"github.com/GizClaw/flowcraft/memory/text/timex"
	whenadp "github.com/GizClaw/flowcraft/memory/text/timex/adapter/when"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

const (
	maxEvidenceRescues      = 6
	minEvidenceRescueScore  = 0.30
	evidenceReplaceMargin   = 0.04
	duplicateJaccardCutoff  = 0.86
	contextDedupeQueryFloor = 0.20
)

type evidenceCandidate struct {
	hit       domain.Hit
	score     float64
	queryRank int
}

type finalSelectionQueryFeatures struct {
	tokens           map[string]struct{}
	numeric          map[string]struct{}
	quoted           map[string]struct{}
	proper           map[string]struct{}
	hasTimeSignal    bool
	hasNumericIntent bool
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
}

func selectFinalEvidenceAwareHits(query string, ordered []domain.Hit, pool []domain.Hit, cap int, hybridRerank bool) []domain.Hit {
	if cap <= 0 || len(ordered) == 0 {
		return ordered
	}
	if hybridRerank {
		return selectFinalHybridRerankHits(query, ordered, pool, cap)
	}
	return selectFinalEvidenceRescueHits(query, ordered, pool, cap)
}

func selectFinalEvidenceRescueHits(query string, ordered []domain.Hit, pool []domain.Hit, cap int) []domain.Hit {
	primary := dedupeHitsByEvidence(query, ordered)
	if len(primary) > cap {
		primary = primary[:cap]
	}
	selected := make([]domain.Hit, 0, cap)
	selected = append(selected, primary...)

	selectedIDs := selectedFactIDs(selected)
	candidates := evidenceCandidates(query, pool, selectedIDs)
	rescues := 0
	for _, cand := range candidates {
		if cand.score < minEvidenceRescueScore || rescues >= maxEvidenceRescues {
			break
		}
		if hitDuplicateInSelection(query, cand.hit, selected) {
			continue
		}
		if len(selected) < cap {
			selected = append(selected, cand.hit)
			selectedIDs[cand.hit.Fact.ID] = struct{}{}
			rescues++
			continue
		}
		idx, minScore := weakestEvidenceHit(query, selected)
		if idx < 0 || cand.score < minScore+evidenceReplaceMargin {
			continue
		}
		delete(selectedIDs, selected[idx].Fact.ID)
		selected[idx] = cand.hit
		selectedIDs[cand.hit.Fact.ID] = struct{}{}
		rescues++
	}
	return selected
}

func selectFinalHybridRerankHits(query string, ordered []domain.Hit, pool []domain.Hit, cap int) []domain.Hit {
	candidatePool := mergeFinalSelectionPool(ordered, pool)
	candidates := finalSelectionCandidates(query, candidatePool)
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

func finalSelectionCandidates(query string, hits []domain.Hit) []finalSelectionCandidate {
	queryFeatures := newFinalSelectionQueryFeatures(query)
	maxScore := 0.0
	for _, hit := range hits {
		if hit.Score > maxScore {
			maxScore = hit.Score
		}
	}
	out := make([]finalSelectionCandidate, 0, len(hits))
	seenFacts := map[string]struct{}{}
	seenEvidence := map[string]struct{}{}
	for i, hit := range hits {
		if hit.Fact.ID != "" {
			if _, ok := seenFacts[hit.Fact.ID]; ok {
				continue
			}
			seenFacts[hit.Fact.ID] = struct{}{}
		}
		evidenceKey := primaryEvidenceKey(hit)
		if evidenceKey != "" {
			if _, ok := seenEvidence[evidenceKey]; ok {
				continue
			}
			seenEvidence[evidenceKey] = struct{}{}
		}
		out = append(out, newFinalSelectionCandidate(queryFeatures, hit, i, maxScore, evidenceKey))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if math.Abs(out[i].score-out[j].score) > 1e-9 {
			return out[i].score > out[j].score
		}
		return betterFinalSelectionTieBreak(out[i], out[j])
	})
	return out
}

func newFinalSelectionQueryFeatures(query string) finalSelectionQueryFeatures {
	anchor := time.Now()
	return finalSelectionQueryFeatures{
		tokens:           tokenSet(tokenize.Detect(query).Tokenize(query)),
		numeric:          numericTokens(query),
		quoted:           quotedTokenSet(query),
		proper:           properNounSet(query),
		hasTimeSignal:    hasTemporalQuestionCue(query) || hasTimex(query, anchor),
		hasNumericIntent: queryHasNumericIntent(query),
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
	evidenceTokens := tokenSet(tokenize.Detect(evidenceText).Tokenize(evidenceText))
	factTokens := tokenSet(tokenize.Detect(factText).Tokenize(factText))
	evidenceNumeric := numericTokens(evidenceText)
	factNumeric := numericTokens(factText)
	evidenceQuoted := quotedTokenSet(evidenceText)
	factQuoted := quotedTokenSet(factText)
	evidenceProper := properNounSet(evidenceText)
	factProper := properNounSet(factText)
	hasNumeric := len(evidenceNumeric) > 0 || len(factNumeric) > 0
	hasTime := false
	if queryFeatures.hasTimeSignal {
		hasTime = hitHasTimeSignal(hit, combinedText)
	}
	evidenceScore := finalSelectionTextScore(queryFeatures, evidenceTokens, evidenceNumeric, evidenceQuoted, evidenceProper, hasTime, len(evidenceNumeric) > 0)
	factScore := finalSelectionTextScore(queryFeatures, factTokens, factNumeric, factQuoted, factProper, hasTime, len(factNumeric) > 0)
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
	if query.hasTimeSignal && candidate.hasTimeSignal {
		score += 0.04
	}
	if query.hasNumericIntent && candidate.hasNumeric {
		score += 0.04
	}
	return score
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

func evidenceCandidates(query string, hits []domain.Hit, selected map[string]struct{}) []evidenceCandidate {
	out := make([]evidenceCandidate, 0, len(hits))
	for i, hit := range hits {
		if hit.Fact.ID != "" {
			if _, ok := selected[hit.Fact.ID]; ok {
				continue
			}
		}
		score := queryEvidenceScore(query, hit)
		if score <= 0 {
			continue
		}
		out = append(out, evidenceCandidate{hit: hit, score: score, queryRank: i})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if math.Abs(out[i].score-out[j].score) > 1e-9 {
			return out[i].score > out[j].score
		}
		if out[i].hit.Score != out[j].hit.Score {
			return out[i].hit.Score > out[j].hit.Score
		}
		return out[i].queryRank < out[j].queryRank
	})
	return out
}

func dedupeHitsByEvidence(query string, hits []domain.Hit) []domain.Hit {
	if len(hits) == 0 {
		return nil
	}
	out := make([]domain.Hit, 0, len(hits))
	seenFacts := map[string]struct{}{}
	seenEvidence := map[string]struct{}{}
	for _, hit := range hits {
		if hit.Fact.ID != "" {
			if _, ok := seenFacts[hit.Fact.ID]; ok {
				continue
			}
		}
		if key := primaryEvidenceKey(hit); key != "" {
			if _, ok := seenEvidence[key]; ok {
				continue
			}
			seenEvidence[key] = struct{}{}
		}
		if hitDuplicateInSelection(query, hit, out) {
			continue
		}
		if hit.Fact.ID != "" {
			seenFacts[hit.Fact.ID] = struct{}{}
		}
		out = append(out, hit)
	}
	return out
}

func hitDuplicateInSelection(query string, hit domain.Hit, selected []domain.Hit) bool {
	key := primaryEvidenceKey(hit)
	for _, existing := range selected {
		if hit.Fact.ID != "" && hit.Fact.ID == existing.Fact.ID {
			return true
		}
		if key != "" && key == primaryEvidenceKey(existing) {
			return true
		}
		if sameStructuredMemory(hit.Fact, existing.Fact) && evidenceTextJaccard(hit, existing) >= duplicateJaccardCutoff {
			// Only collapse near-duplicates when the replacement evidence is
			// not strongly query-specific; exact evidence/query matches still
			// deserve a chance to surface.
			if queryEvidenceScore(query, hit) < contextDedupeQueryFloor {
				return true
			}
		}
	}
	return false
}

func weakestEvidenceHit(query string, hits []domain.Hit) (int, float64) {
	if len(hits) == 0 {
		return -1, 0
	}
	idx := 0
	minScore := queryEvidenceScore(query, hits[0])
	for i := 1; i < len(hits); i++ {
		score := queryEvidenceScore(query, hits[i])
		if score < minScore || (score == minScore && hits[i].Score < hits[idx].Score) {
			idx = i
			minScore = score
		}
	}
	return idx, minScore
}

func selectedFactIDs(hits []domain.Hit) map[string]struct{} {
	out := make(map[string]struct{}, len(hits))
	for _, hit := range hits {
		if hit.Fact.ID != "" {
			out[hit.Fact.ID] = struct{}{}
		}
	}
	return out
}

func queryEvidenceScore(query string, hit domain.Hit) float64 {
	return queryTextScore(query, hitEvidenceText(hit), hit)
}

func queryFactScore(query string, hit domain.Hit) float64 {
	return queryTextScore(query, finalSelectionFactText(hit), hit)
}

func queryTextScore(query string, text string, hit domain.Hit) float64 {
	queryTokens := tokenSet(tokenize.Detect(query).Tokenize(query))
	if len(queryTokens) == 0 {
		return 0
	}
	textTokens := tokenSet(tokenize.Detect(text).Tokenize(text))
	if len(textTokens) == 0 {
		return 0
	}
	matched := 0
	for tok := range queryTokens {
		if _, ok := textTokens[tok]; ok {
			matched++
		}
	}
	coverage := float64(matched) / float64(len(queryTokens))
	score := coverage
	if numericOverlap(query, text) {
		score += 0.25
	}
	if queryHasNumericIntent(query) && hasNumericEvidence(text) {
		score += 0.15
	}
	if queryHasTimeSignal(query) && hitHasTimeSignal(hit, text) {
		score += 0.20
	}
	if quotedOverlap(query, text) {
		score += 0.15
	}
	if properNounOverlap(query, text) {
		score += 0.10
	}
	if score > 1 {
		return 1
	}
	return score
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

func finalSelectionHitText(hit domain.Hit) string {
	fact := finalSelectionFactText(hit)
	evidence := hitEvidenceText(hit)
	if fact == "" {
		return evidence
	}
	if evidence == "" {
		return fact
	}
	return fact + " " + evidence
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

func evidenceTextJaccard(a, b domain.Hit) float64 {
	aText := hitEvidenceText(a)
	bText := hitEvidenceText(b)
	return tokenSetJaccard(
		tokenSet(tokenize.Detect(aText).Tokenize(aText)),
		tokenSet(tokenize.Detect(bText).Tokenize(bText)),
	)
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

func tokenSet(tokens []string) map[string]struct{} {
	out := make(map[string]struct{}, len(tokens))
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		out[tok] = struct{}{}
	}
	return out
}

func numericOverlap(a, b string) bool {
	an := numericTokens(a)
	if len(an) == 0 {
		return false
	}
	for n := range numericTokens(b) {
		if _, ok := an[n]; ok {
			return true
		}
	}
	return false
}

func numericTokens(text string) map[string]struct{} {
	out := map[string]struct{}{}
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		out[cur.String()] = struct{}{}
		cur.Reset()
	}
	for _, r := range text {
		if unicode.IsDigit(r) || unicode.IsNumber(r) {
			cur.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

func queryHasTimeSignal(query string) bool {
	if hasTemporalQuestionCue(query) {
		return true
	}
	return hasTimex(query, time.Now())
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
	return hasTimex(evidence, time.Now())
}

func hasTimex(text string, anchor time.Time) bool {
	if m, err := (timex.RegexParser{}).Parse(text, anchor); err == nil && m != nil {
		return true
	}
	p, err := finalSelectionTimeParser()
	if err != nil || p == nil {
		return false
	}
	m, err := p.Parse(text, anchor)
	return err == nil && m != nil
}

var finalSelectionTimeParser = sync.OnceValues(func() (timex.Parser, error) {
	return whenadp.NewWithLanguages("en", "zh", "nl", "ru", "br")
})

func hasTemporalQuestionCue(query string) bool {
	q := strings.ToLower(query)
	for _, cue := range []string{
		"when", "what date", "which day", "how long", "how old",
		"earliest", "latest", "before", "after", "during",
		"什么时候", "哪天", "多久", "多长时间", "最早", "最晚",
	} {
		if strings.Contains(q, cue) {
			return true
		}
	}
	return false
}

func queryHasNumericIntent(query string) bool {
	q := strings.ToLower(query)
	for _, cue := range []string{"how many", "how much", "number", "count", "total", "多少", "几个", "几次"} {
		if strings.Contains(q, cue) {
			return true
		}
	}
	return false
}

func hasNumericEvidence(text string) bool {
	for _, r := range text {
		if unicode.IsDigit(r) || unicode.IsNumber(r) {
			return true
		}
	}
	return false
}

func quotedOverlap(a, b string) bool {
	qa := quotedTokenSet(a)
	if len(qa) == 0 {
		return false
	}
	qb := quotedTokenSet(b)
	if len(qb) == 0 {
		qb = tokenSet(tokenize.Detect(b).Tokenize(b))
	}
	for tok := range qa {
		if _, ok := qb[tok]; ok {
			return true
		}
	}
	return false
}

func quotedTokenSet(text string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, span := range quotes.ExtractSpans(text) {
		for tok := range tokenSet(tokenize.Detect(span).Tokenize(span)) {
			out[tok] = struct{}{}
		}
	}
	return out
}

func properNounOverlap(a, b string) bool {
	pa := properNounSet(a)
	if len(pa) == 0 {
		return false
	}
	pb := properNounSet(b)
	for tok := range pa {
		if _, ok := pb[tok]; ok {
			return true
		}
	}
	return false
}

func properNounSet(text string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, tok := range tokenize.SplitProperNouns(text) {
		if !isTitleCased(tok) {
			continue
		}
		out[strings.ToLower(tok)] = struct{}{}
	}
	return out
}

func isTitleCased(tok string) bool {
	if len(tok) < 2 {
		return false
	}
	runes := []rune(tok)
	if !unicode.IsUpper(runes[0]) {
		return false
	}
	for _, r := range runes[1:] {
		if unicode.IsLower(r) {
			return true
		}
	}
	return false
}
