package normalize

import (
	"strings"
	"unicode"
)

// IsDigitString reports whether s is a non-empty sequence of Unicode digits.
func IsDigitString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// TrimLeadingASCIIZeros normalizes an ASCII digit span for equality checks.
// If the span is all zeros, it returns "0".
func TrimLeadingASCIIZeros(s string) string {
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "0"
	}
	return s
}
