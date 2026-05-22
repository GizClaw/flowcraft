package tokenize

import "unicode"

// IsCJK reports whether r is a CJK character — Han (Chinese), Hangul
// (Korean), Katakana or Hiragana (Japanese). It is the cheap script
// sniffer the rest of the package uses to decide between ASCII /
// CJK code paths.
//
// The check excludes CJK punctuation and ideographic numerals on
// purpose: those code points behave like ASCII separators at the
// tokenizer level (they segment words rather than form them).
func IsCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hangul, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hiragana, r)
}
