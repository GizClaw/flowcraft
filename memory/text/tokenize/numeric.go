package tokenize

import (
	"strings"
	"unicode"
)

// SplitNumbers returns contiguous Unicode digit spans from text.
// It does not parse or interpret the numbers; callers own any domain
// filtering such as excluding dates or timestamps.
func SplitNumbers(text string) []string {
	if text == "" {
		return nil
	}
	out := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsDigit(r)
	})
	if len(out) == 0 {
		return nil
	}
	return out
}
