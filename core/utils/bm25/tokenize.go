package bm25

import (
	"strings"
	"unicode"
)

// Tokenize splits text into search terms: runs of letters and digits,
// lowercased, plus CJK characters and CJK bigrams. Bigrams let a
// Chinese query match a fragment without a segmentation dictionary,
// while the single characters keep leftover fragments matchable.
// Single-character words and digits are kept; only symbols and
// whitespace are dropped, and no stopword list is applied.
//
// Tokenize is the tokenizer both the index and its queries use. A
// consumer that persists tokenized fields for a TermDoc should build
// them with Tokenize (or an equivalent splitter) so later queries can
// still match.
func Tokenize(s string) []string {
	s = strings.ToLower(s)
	var tokens []string
	var word strings.Builder
	var prevCJK rune
	flushWord := func() {
		if word.Len() > 0 {
			tokens = append(tokens, word.String())
			word.Reset()
		}
	}
	for _, r := range s {
		switch {
		case isCJK(r):
			flushWord()
			if prevCJK != 0 {
				tokens = append(tokens, string(prevCJK)+string(r))
			}
			tokens = append(tokens, string(r))
			prevCJK = r
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			word.WriteRune(r)
			prevCJK = 0
		default:
			flushWord()
			prevCJK = 0
		}
	}
	flushWord()
	return tokens
}

// isCJK covers the scripts that have no whitespace word boundaries:
// Han, Hiragana, Katakana and Hangul.
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}
