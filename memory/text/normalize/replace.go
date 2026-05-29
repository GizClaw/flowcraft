package normalize

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// ReplaceStandaloneFold replaces case-insensitive standalone token surfaces
// while preserving the surrounding text.
func ReplaceStandaloneFold(text, token, replacement string) string {
	if text == "" || token == "" {
		return text
	}
	var out strings.Builder
	for i := 0; i < len(text); {
		if hasPrefixFold(text[i:], token) && standaloneTokenBoundary(text, i, i+len(token)) {
			out.WriteString(replacement)
			i += len(token)
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		out.WriteRune(r)
		i += size
	}
	return out.String()
}

func standaloneTokenBoundary(text string, start, end int) bool {
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(text[:start])
		if isTokenContinuation(r) {
			return false
		}
	}
	if end < len(text) {
		r, _ := utf8.DecodeRuneInString(text[end:])
		if isTokenContinuation(r) {
			return false
		}
	}
	return true
}

func isTokenContinuation(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '\'' || r == '’' || r == '-'
}

func hasPrefixFold(text, prefix string) bool {
	return len(text) >= len(prefix) && strings.EqualFold(text[:len(prefix)], prefix)
}
