package html

import (
	"html"
	"regexp"
	"strings"
	"unicode"
)

// CleanResult contains the result of text cleaning.
type CleanResult struct {
	Text            string
	TotalCharacters int
	WasTruncated    bool
}

// Normalize normalizes Unicode text: removes invisible characters,
// normalizes per-line whitespace, and merges consecutive blank lines.
func Normalize(s string) string {
	s = removeInvisibleChars(s)

	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.Join(strings.Fields(line), " ")
	}
	s = strings.Join(lines, "\n")

	s = mergeNewlines(s)
	return strings.TrimSpace(s)
}

func removeInvisibleChars(s string) string {
	var result strings.Builder
	result.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			result.WriteRune(r)
		case r == '\u00A0':
			result.WriteRune(' ')
		case r == '\uFEFF':
			continue
		case unicode.Is(unicode.Cf, r):
			continue
		case unicode.IsControl(r):
			continue
		default:
			result.WriteRune(r)
		}
	}
	return result.String()
}

var multiNewline = regexp.MustCompile(`\n{3,}`)

func mergeNewlines(s string) string {
	return multiNewline.ReplaceAllString(s, "\n\n")
}

// ApplyBudget truncates text at sentence boundaries to stay within maxChars.
// Returns TotalCharacters (pre-truncation length) for callers to measure loss.
// If maxChars <= 0, no truncation is applied.
func ApplyBudget(text string, maxChars int) CleanResult {
	total := len([]rune(text))
	if maxChars <= 0 || total <= maxChars {
		return CleanResult{Text: text, TotalCharacters: total, WasTruncated: false}
	}

	runes := []rune(text)
	truncated := string(runes[:maxChars])

	lastPunct := strings.LastIndexAny(truncated, ".!。!？?")
	lastNewline := strings.LastIndex(truncated, "\n\n")

	breakPoint := lastPunct
	if breakPoint < 0 || (lastNewline > breakPoint && lastNewline > 0) {
		breakPoint = lastNewline
	}
	halfBytes := len(truncated) / 2
	if breakPoint < halfBytes {
		breakPoint = len(truncated)
	} else {
		breakPoint++
	}

	return CleanResult{
		Text:            strings.TrimSpace(truncated[:breakPoint]),
		TotalCharacters: total,
		WasTruncated:    true,
	}
}

// DecodeHtmlEntities decodes HTML entities to regular characters.
func DecodeHtmlEntities(s string) string {
	return html.UnescapeString(s)
}

// CleanString applies all cleaning steps to a string.
func CleanString(s string, maxChars int) CleanResult {
	normalized := Normalize(s)
	decoded := DecodeHtmlEntities(normalized)
	return ApplyBudget(decoded, maxChars)
}
