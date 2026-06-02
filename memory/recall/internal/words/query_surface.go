package words

import (
	"strings"

	"github.com/GizClaw/flowcraft/memory/text/stopword"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

var querySurfaceStopwords = stopword.MultilingualSet().Extend(
	"am", "hers", "ours", "theirs",
)

var temporalQuestionWords = map[string]struct{}{
	"when": {}, "date": {}, "time": {}, "did": {}, "does": {}, "was": {}, "were": {}, "is": {}, "are": {},
	"cuando": {}, "cuándo": {}, "fecha": {}, "tiempo": {}, "fue": {}, "eran": {},
	"quand": {}, "temps": {}, "était": {}, "etait": {},
	"wann": {}, "datum": {}, "zeit": {}, "war": {}, "waren": {},
	"quando": {}, "data": {}, "tempo": {}, "foi": {},
	"wanneer": {}, "tijd": {},
	"когда": {}, "дата": {}, "время": {}, "был": {}, "была": {}, "были": {},
}

func splitQueryWords(text string) []string {
	return tokenize.SplitWords(text)
}

func isQueryStopword(word string) bool {
	word = strings.TrimSpace(word)
	return len([]rune(word)) <= 1 || querySurfaceStopwords.Contains(word)
}

func significantQueryTerms(text string) []string {
	words := splitQueryWords(text)
	out := make([]string, 0, len(words))
	for _, word := range words {
		if isQueryStopword(word) {
			continue
		}
		out = append(out, word)
	}
	return out
}

// SignificantQueryText returns a compact query variant or the original text
// when every term is filtered.
func SignificantQueryText(text string) string {
	terms := significantQueryTerms(text)
	if len(terms) == 0 {
		return text
	}
	return strings.Join(terms, " ")
}

// StripTemporalQuestionWords removes generic temporal question words for
// source-expansion variants.
func StripTemporalQuestionWords(text string) string {
	words := splitQueryWords(text)
	out := make([]string, 0, len(words))
	for _, word := range words {
		if _, ok := temporalQuestionWords[strings.ToLower(word)]; ok {
			continue
		}
		out = append(out, word)
	}
	return strings.Join(out, " ")
}
