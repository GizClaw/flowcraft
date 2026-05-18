package recall

import (
	"sort"
	"strings"
	"unicode"
)

// NormalizeEntities expands phrasal entity mentions returned by the
// extractor into individual proper-noun atoms so query-time entity
// lookup (which works at token granularity, see
// sdk/retrieval/pipeline/stages_query.go ruleEntities) can match
// them.
//
// Background: extractor LLMs return entities as they appear in the
// dialog — "Mira's photography club", "Tuesday morning
// meeting", "my sister Alice", etc. The query side rule-based
// extractor on the other hand walks the question and emits
// individual capitalised + CJK atoms — for the same conversation
// it would emit ["mira", "photography", "tuesday", "alice"]. Without
// reconciliation the stored list and the query list don't share a
// string, so the entity retrieval lane's ContainsAny filter never
// fires and the lane silently degrades to zero recall in entity-dense
// conversational workloads.
//
// NormalizeEntities folds both representations onto the same key
// space. For each LLM-supplied entity it:
//
//   - Lowercases and trims surrounding whitespace / punctuation,
//     keeping the (now-normalised) phrase as one atom — preserves
//     direct match against query atoms that happen to be the full
//     phrase ("New York", "李华", "黑咖啡").
//   - Splits the phrase on whitespace and punctuation, then keeps
//     individual tokens that look like proper nouns or CJK words
//     (capitalised in the original, or any CJK rune; min length 2),
//     lowercased — produces the per-token atoms that match the
//     query-side extractor.
//   - Drops question-pronoun atoms ("what", "where", "when", "why",
//     "who", "how") that leak into capitalised-token extraction
//     when an entity phrase starts with one (e.g. "What Day").
//
// Output is sorted + deduplicated.
func NormalizeEntities(in []string) []string {
	set := map[string]struct{}{}
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		// 1. Keep the full phrase in normalised form.
		phrase := strings.ToLower(strings.TrimFunc(s, isTrimmableRune))
		if len([]rune(phrase)) >= 2 && !isQuestionPronoun(phrase) {
			set[phrase] = struct{}{}
		}
		// 2. Split into per-token atoms.
		for _, w := range strings.FieldsFunc(s, isEntitySplitRune) {
			tok := strings.TrimFunc(w, isTrimmableRune)
			if tok == "" {
				continue
			}
			runes := []rune(tok)
			if len(runes) < 2 {
				continue
			}
			isCapAscii := unicode.IsUpper(runes[0])
			hasCJK := false
			for _, r := range runes {
				if isCJKRune(r) {
					hasCJK = true
					break
				}
			}
			if !isCapAscii && !hasCJK {
				continue
			}
			low := strings.ToLower(tok)
			// Drop English possessive "'s" so "Mira's" and
			// "Mira" share an atom — common in dialog extractor
			// output where the LLM preserves the apostrophe form.
			low = strings.TrimSuffix(low, "'s")
			low = strings.TrimSuffix(low, "\u2019s") // typographic apostrophe
			if isQuestionPronoun(low) {
				continue
			}
			set[low] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func isTrimmableRune(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsPunct(r)
}

func isEntitySplitRune(r rune) bool {
	// Split on whitespace and most punctuation. Keep apostrophes and
	// hyphens inside the token so "O'Brien" / "vice-president" stay
	// joined (the apostrophe will be re-stripped above for "Mira's").
	if unicode.IsSpace(r) {
		return true
	}
	if unicode.IsPunct(r) && r != '\'' && r != '-' && r != '\u2019' {
		return true
	}
	return false
}

// isCJKRune mirrors sdk/textsearch.IsCJK without forcing this package
// to depend on textsearch — the entity-normalisation hot path runs on
// every ingest and the indirection isn't worth it for one rune check.
func isCJKRune(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hangul, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hiragana, r)
}

var questionPronouns = map[string]bool{
	"what": true, "who": true, "whom": true, "whose": true,
	"when": true, "where": true, "why": true, "how": true,
	"which": true, "the": true, "an": true, "a": true,
}

func isQuestionPronoun(s string) bool { return questionPronouns[s] }
