package recall

import (
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
)

func fusionCandidateCap(finalCap int) int {
	if finalCap <= 0 {
		finalCap = planner.DefaultLimit
	}
	cap := finalCap * planner.SourceOverfetchMultiplier
	if cap > planner.MaxSourceOverfetch {
		cap = planner.MaxSourceOverfetch
	}
	if cap < finalCap {
		cap = finalCap
	}
	return cap
}

func rankContextItems(items []domain.ContextItem, intent domain.QueryIntent, finalCap int) []domain.ContextItem {
	if len(items) == 0 {
		return items
	}
	terms := significantQueryTerms(intent.Text)
	for i := range items {
		boost := factRankBoost(items[i], intent, terms)
		if boost == 0 {
			continue
		}
		items[i].Candidate.Score += boost
		if items[i].Candidate.Metadata == nil {
			items[i].Candidate.Metadata = map[string]any{}
		}
		items[i].Candidate.Metadata["rank_boost"] = boost
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Candidate.Score > items[j].Candidate.Score
	})
	if finalCap > 0 && len(items) > finalCap {
		items = items[:finalCap]
	}
	return items
}

func factRankBoost(item domain.ContextItem, intent domain.QueryIntent, terms []string) float64 {
	f := item.Fact
	var boost float64
	termMatches := factTermMatchCount(f, terms)
	hasTextMatch := termMatches > 0
	if hasTextMatch && factMatchesAnyEntity(f, intent.Entities) {
		boost += 0.015
	}
	if hasTextMatch && intent.Subject != "" && factMatchesEntity(f, intent.Subject) {
		boost += 0.015
	}
	if hasTextMatch && factKindMatches(f.Kind, intent.Kinds) {
		boost += 0.012
	}
	if hasTextMatch && temporalIntent(intent) {
		if f.ValidFrom != nil || !f.ObservedAt.IsZero() {
			boost += 0.006
		}
		if candidateHasSource(item.Candidate, planner.SourceTimeline) {
			boost += 0.008
		}
	}
	if hasTextMatch && planner.ActivatesRelation(intent) && candidateHasSource(item.Candidate, planner.SourceRelation) {
		boost += 0.01
	}
	if hasTextMatch && planner.ActivatesProfile(intent) && candidateHasSource(item.Candidate, planner.SourceProfile) {
		boost += 0.01
	}
	boost += termMatchBoost(termMatches)
	if f.Confidence > 0 {
		c := f.Confidence
		if c > 1 {
			c = 1
		}
		boost += c * 0.004
	}
	return boost
}

func temporalIntent(intent domain.QueryIntent) bool {
	if !intent.TimeRange.IsZero() {
		return true
	}
	for _, k := range intent.Kinds {
		switch k {
		case domain.KindEvent, domain.KindState, domain.KindPlan:
			return true
		}
	}
	return false
}

func factKindMatches(kind domain.FactKind, kinds []domain.FactKind) bool {
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}

func factMatchesAnyEntity(f domain.TemporalFact, entities []string) bool {
	for _, e := range entities {
		if factMatchesEntity(f, e) {
			return true
		}
	}
	return false
}

func factMatchesEntity(f domain.TemporalFact, entity string) bool {
	entity = normalizeRankText(entity)
	if entity == "" {
		return false
	}
	for _, s := range []string{f.Subject, f.Object, f.Location} {
		if normalizeRankText(s) == entity {
			return true
		}
	}
	for _, e := range f.Entities {
		if normalizeRankText(e) == entity {
			return true
		}
	}
	for _, p := range f.Participants {
		if normalizeRankText(p) == entity {
			return true
		}
	}
	return false
}

func candidateHasSource(c domain.Candidate, source string) bool {
	if c.Source == source {
		return true
	}
	if c.Metadata == nil {
		return false
	}
	sources, _ := c.Metadata["sources"].([]string)
	return slices.Contains(sources, source)
}

// factTermMatchCount counts how many significant query terms appear
// in the fact's canonical fields. Matches in Content / S / P / O
// count double because those fields are the LLM's deliberate
// summary of the fact; matches in evidence quotes count single
// because evidence text often carries unrelated chat chatter that
// happens to share a term with the query. Returning a weighted
// count lets `termMatchBoost` push fact-content matches above
// merely-coincidental evidence matches.
func factTermMatchCount(f domain.TemporalFact, terms []string) int {
	if len(terms) == 0 {
		return 0
	}
	primary := strings.ToLower(f.Content + " " + f.Subject + " " + f.Predicate + " " + f.Object)
	var secondary strings.Builder
	secondary.WriteString(strings.ToLower(f.EvidenceText))
	for _, ref := range f.EvidenceRefs {
		if ref.Text != "" {
			secondary.WriteString(" " + strings.ToLower(ref.Text))
		}
	}
	matches := 0
	for _, term := range terms {
		if strings.Contains(primary, term) {
			matches += 2
			continue
		}
		if strings.Contains(secondary.String(), term) {
			matches++
		}
	}
	if matches > 10 {
		matches = 10
	}
	return matches
}

// termMatchBoost converts the weighted match count into a score
// delta. The unit step is 0.006 — slightly above one σ of the
// typical BM25 score gap between adjacent top-10 candidates
// (≈ 0.005), so a single content-field match is the smallest
// signal that can move ranking, while a multi-term content match
// (boost ≥ 0.024) is decisive.
func termMatchBoost(matches int) float64 {
	if matches <= 0 {
		return 0
	}
	return float64(matches) * 0.006
}

// significantQueryTerms tokenises the query and keeps only terms
// that are likely to carry content-word signal. Terms shorter than
// 3 characters or matching the closed-class stopword table are
// filtered. Duplicate terms within a single query collapse so that
// "books books" does not double-count.
func significantQueryTerms(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := map[string]struct{}{}
	var out []string
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if len(f) < 3 || isRankStopword(f) {
			continue
		}
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out
}

func normalizeRankText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// isRankStopword is the curated stopword table the ranker uses to
// drop function words from query-term significance scoring. Without
// it, terms like "has" or "have" collide with the content of every
// English fact ("Melanie has a dog", "Melanie has read books") and
// nullify the differentiating signal coming from real content words
// like "books" or "read". The list is deliberately conservative:
// only closed-class function words and Wh-words are stripped;
// nominal / verbal stems stay so domain vocabulary still scores.
func isRankStopword(s string) bool {
	switch s {
	case
		// Wh-words
		"who", "whom", "whose", "what", "when", "where", "why", "how", "which",
		// Auxiliaries
		"is", "am", "are", "was", "were", "be", "been", "being",
		"do", "does", "did", "done",
		"have", "has", "had", "having",
		"will", "would", "shall", "should", "can", "could", "may", "might", "must",
		// Articles / determiners / quantifiers
		"the", "a", "an", "this", "that", "these", "those",
		"some", "any", "all", "each", "every", "no", "none",
		// Conjunctions / connectives
		"and", "but", "or", "nor", "so", "yet",
		"if", "then", "than", "because", "while", "though",
		// Common prepositions
		"of", "in", "on", "at", "by", "to", "for", "with", "without",
		"from", "into", "onto", "out", "off", "over", "under", "between",
		"among", "about", "across", "around", "through", "during", "before", "after",
		// Pronouns
		"i", "me", "my", "mine", "myself",
		"you", "your", "yours", "yourself",
		"he", "him", "his", "himself",
		"she", "her", "hers", "herself",
		"it", "its", "itself",
		"we", "us", "our", "ours", "ourselves",
		"they", "them", "their", "theirs", "themselves",
		// Misc fillers
		"there", "here", "as", "such", "very", "much", "many", "more", "most",
		"just", "only", "also", "too", "either", "neither":
		return true
	}
	return false
}
