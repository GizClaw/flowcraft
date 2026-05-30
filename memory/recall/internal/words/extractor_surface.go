package words

import (
	"strings"
	"unicode"

	"github.com/GizClaw/flowcraft/memory/text/normalize"
)

type ExtractorContentRewrite struct {
	Token       string
	Prefix      string
	Replacement string
}

func FirstPersonSingularExtractorContentPrefixRewrites(subject string) []ExtractorContentRewrite {
	return []ExtractorContentRewrite{
		{Prefix: "I'm ", Replacement: subject + " is "},
		{Prefix: "I’m ", Replacement: subject + " is "},
		{Prefix: "I am ", Replacement: subject + " is "},
		{Prefix: "I've ", Replacement: subject + " has "},
		{Prefix: "I’ve ", Replacement: subject + " has "},
		{Prefix: "I have ", Replacement: subject + " has "},
		{Prefix: "I'll ", Replacement: subject + " will "},
		{Prefix: "I’ll ", Replacement: subject + " will "},
		{Prefix: "I will ", Replacement: subject + " will "},
		{Prefix: "I was ", Replacement: subject + " was "},
		{Prefix: "I had ", Replacement: subject + " had "},
		{Prefix: "My ", Replacement: subject + "'s "},
	}
}

func EmbeddedFirstPersonSingularExtractorContentRewrites(subject string) []ExtractorContentRewrite {
	return []ExtractorContentRewrite{
		{Token: "I'm", Replacement: subject + " is"},
		{Token: "I’m", Replacement: subject + " is"},
		{Token: "I am", Replacement: subject + " is"},
		{Token: "I've", Replacement: subject + " has"},
		{Token: "I’ve", Replacement: subject + " has"},
		{Token: "I have", Replacement: subject + " has"},
		{Token: "I'll", Replacement: subject + " will"},
		{Token: "I’ll", Replacement: subject + " will"},
		{Token: "I will", Replacement: subject + " will"},
		{Token: "I'd", Replacement: subject + " would"},
		{Token: "I’d", Replacement: subject + " would"},
		{Token: "I", Replacement: subject},
		{Token: "me", Replacement: subject},
		{Token: "mine", Replacement: subject + "'s"},
		{Token: "myself", Replacement: subject},
		{Token: "my", Replacement: subject + "'s"},
	}
}

func IsFirstPersonSingularExtractorSubjectText(subject string) bool {
	tokens := strings.Fields(CanonicalSurface(subject))
	return IsFirstPersonSingularExtractorSubject(tokens)
}

func IsWeakExtractorEntityText(s string) bool {
	return IsWeakExtractorEntityPhrase(strings.Fields(CanonicalSurface(s)))
}

func HasExtractorUppercase(s string) bool {
	for _, r := range s {
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

func IsExtractorAllCapsAnchor(s string) bool {
	s = strings.TrimSpace(s)
	if len([]rune(s)) < 2 {
		return false
	}
	hasLetter := false
	for _, r := range s {
		if !unicode.IsLetter(r) {
			continue
		}
		hasLetter = true
		if unicode.IsLower(r) {
			return false
		}
	}
	return hasLetter
}

func NormalizeExtractorEvidenceAnchor(s string) string {
	return strings.ToLower(normalize.CollapseSpaces(normalize.ReplaceNonAlnumWithSpace(s)))
}
