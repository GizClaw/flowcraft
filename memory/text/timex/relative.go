package timex

import "strings"

// IsRelativePhrase reports whether raw looks like a relative time expression
// rather than an absolute calendar date. It is intentionally lexical: callers
// should still use a Parser to resolve the expression to a timestamp.
func IsRelativePhrase(raw string) bool {
	token := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(raw)), " "))
	if token == "" {
		return false
	}
	switch token {
	case "now", "today", "tomorrow", "yesterday",
		"next week", "last week", "next month", "last month", "next year", "last year":
		return true
	}
	return strings.Contains(token, " ago") ||
		strings.Contains(token, " from now") ||
		strings.HasPrefix(token, "in ") ||
		strings.Contains(token, "前") ||
		strings.Contains(token, "后")
}
