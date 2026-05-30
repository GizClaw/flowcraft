// Package normalize provides Unicode-aware string canonicalisation
// primitives used by the sdk/text family and any downstream component
// that needs schema-grade equality on free-form text columns.
//
// The package keeps three orthogonal helpers:
//
//   - [NFC] applies Unicode Normalisation Form C so pre-composed and
//     decomposed encodings ("é" vs "e\u0301") collapse onto the same
//     byte sequence. This is the right primitive for stable hashing
//     and merge-key equality in canonical stores.
//   - [CollapseSpaces] folds [NFC] with internal-whitespace collapse
//     and edge trim, producing the standard "user-visible canonical
//     string" form: NFC + single ASCII spaces + no leading/trailing
//     whitespace.
//   - [ReplaceNonAlnumWithSpace] rewrites any non-letter / non-digit
//     rune as ASCII space, which is the right pre-folding for
//     predicate-like identifier columns that should absorb
//     "favorite-color", "favorite/color", "favorite.color" into a
//     single canonical "favorite color" after [CollapseSpaces].
//
// The functions are deterministic and idempotent — re-normalising an
// already-normal string is a no-op. They do NOT lower-case (callers
// layer case folding on top via strings.ToLower) and do NOT consult
// any stop-word table; those decisions belong in the caller.
package normalize

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// NFC applies Unicode Normalisation Form C. The check covers
// pre-composed vs decomposed encodings (e.g. "é" U+00E9 vs
// "e\u0301") and ligature normalisation where the input form differs
// from the canonical composition. Empty input is returned unchanged.
//
// Use NFC anywhere a downstream contract assumes byte-equal Unicode
// surface forms — hashing, merge keys, equality checks across
// caller-supplied data that may have been typed on different
// platforms (macOS prefers NFD on filesystem strings, most other
// platforms prefer NFC).
func NFC(s string) string {
	if s == "" {
		return ""
	}
	return norm.NFC.String(s)
}

// CollapseSpaces returns the canonical user-visible form of s: NFC
// folded + internal whitespace runs collapsed to a single ASCII
// space + edge whitespace trimmed.
//
// This is the right primitive for free-form text columns that should
// support direct equality comparison and stable hashing — Subject /
// Object / Location / Content in canonical stores, log message
// dedup keys, etc.
func CollapseSpaces(s string) string {
	if s == "" {
		return ""
	}
	return strings.Join(strings.Fields(NFC(s)), " ")
}

// ReplaceNonAlnumWithSpace rewrites any rune that is not a Unicode
// letter or digit to a single ASCII space. Run lengths are NOT
// collapsed — callers typically follow with [CollapseSpaces] to get
// the canonical single-space form.
//
// The function is the right pre-folding for predicate / identifier
// columns that should absorb mixed punctuation: "favorite-color",
// "favorite/color", "favorite.color" all map to "favorite color"
// after [CollapseSpaces].
func ReplaceNonAlnumWithSpace(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return b.String()
}
