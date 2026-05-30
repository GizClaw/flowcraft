package words

import (
	"sort"
	"strings"
	"unicode"

	"github.com/GizClaw/flowcraft/memory/text/quotes"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

// NormalizeIntentEntityMention folds a surface mention into the canonical shape
// used by the rule-based recall intent compiler.
func NormalizeIntentEntityMention(s string) string {
	s = strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsPunct(r) || unicode.IsSpace(r)
	})
	if len(s) < 2 {
		return ""
	}
	return strings.ToLower(s)
}

// ExtractIntentEntityMentions is a conservative query-time entity baseline:
// quoted spans, capitalized tokens, and CJK runs filtered by recall stopwords.
func ExtractIntentEntityMentions(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	set := map[string]struct{}{}
	add := func(s string) {
		s = NormalizeIntentEntityMention(s)
		if s == "" || IsIntentEntityStopword(s) {
			return
		}
		set[s] = struct{}{}
	}
	for _, q := range quotes.ExtractSpans(text) {
		add(q)
	}
	for i, w := range splitIntentEntityFields(text) {
		runes := []rune(w)
		if len(runes) < 2 {
			continue
		}
		lower := strings.ToLower(w)
		if i == 0 && IsIntentEntityStopword(lower) {
			continue
		}
		if unicode.IsUpper(runes[0]) && !IsIntentEntityStopword(lower) {
			add(w)
		}
		if hasCJKRunes(w) && len(runes) >= 2 {
			add(w)
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func splitIntentEntityFields(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		// Keep apostrophe and hyphen inside names like O'Brien and Jean-Luc.
		return unicode.IsSpace(r) || (unicode.IsPunct(r) && r != '\'' && r != '-')
	})
}

func hasCJKRunes(s string) bool {
	for _, r := range s {
		if tokenize.IsCJK(r) {
			return true
		}
	}
	return false
}
