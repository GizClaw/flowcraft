package tokenize

import (
	"strings"
	"unicode"
)

// SplitWords splits text on Unicode letter / digit boundaries and
// returns the surface words in original case, without stop-word
// filtering, lemmatisation, or stemming.
//
// SplitWords is the right primitive for callers that need raw word
// boundaries — primarily named-entity heuristics (Title-cased token
// detection, capitalised quoted strings) where folding case or
// dropping short tokens would destroy the signal. BM25 callers
// should reach for [Simple.Tokenize] or [CJKBigram.Tokenize]
// instead.
//
// The function intentionally does NOT lower-case, does NOT drop
// short tokens, and does NOT consult any stop-word table; those
// decisions belong in the caller. The behaviour mirrors
// strings.FieldsFunc with a Unicode-aware boundary predicate so
// non-Latin scripts still split correctly.
func SplitWords(text string) []string {
	if text == "" {
		return nil
	}
	out := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(out) == 0 {
		return nil
	}
	return out
}
