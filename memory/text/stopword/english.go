// Package stopword provides high-frequency-word filters used by the
// sdk/text tokenizers and any downstream component that needs to
// exclude semantically empty words from BM25 vocabularies, NER
// candidate lists, or query intent extraction.
//
// The package keeps two distinct tables:
//
//   - [EnglishSet] / [IsEnglish] for ASCII English (the 136-word
//     baseline used by [sdk/text/tokenize.Simple]).
//   - [IsCJKChar] for high-frequency CJK function characters (the
//     50-character baseline used by
//     [sdk/text/tokenize.CJKBigram]).
//
// The tables are intentionally small. Larger off-the-shelf lists
// (NLTK, spaCy) over-aggressively drop tokens that carry signal in
// memory / conversational workloads ("never", "all", "always");
// callers needing a different policy should construct their own
// [Set] with [Set.Extend] and inject it into the tokenizer.
package stopword

import "strings"

// stopWords is the canonical English stop-word baseline. Callers
// must not mutate the map; use [EnglishSet] to obtain a writable
// copy.
//
// The list was hand-picked to drop semantically empty words while
// preserving cues that BM25 / NER care about: negatives ("not"),
// determiners that pin time ("always", "never"), and quantifiers
// that scope ("some", "every", "any"). It is held to ~136 entries
// on purpose — the table is the hot path on every tokenisation
// call and a smaller map keeps cache behaviour predictable.
var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "is": true, "are": true,
	"was": true, "were": true, "be": true, "been": true, "being": true,
	"have": true, "has": true, "had": true, "do": true, "does": true,
	"did": true, "will": true, "would": true, "could": true, "should": true,
	"of": true, "to": true, "in": true, "for": true, "on": true,
	"with": true, "at": true, "by": true, "from": true, "it": true,
	"this": true, "that": true, "and": true, "or": true, "not": true,
	"i": true, "me": true, "my": true, "we": true, "our": true,
	"you": true, "your": true, "he": true, "she": true, "they": true,
	"but": true, "if": true, "so": true, "no": true, "up": true,
	"out": true, "about": true, "into": true, "than": true, "then": true,
	"its": true, "his": true, "her": true, "their": true, "them": true,
	"him": true, "us": true, "who": true, "which": true, "what": true,
	"when": true, "where": true, "how": true, "all": true, "each": true,
	"every": true, "both": true, "few": true, "more": true, "most": true,
	"other": true, "some": true, "such": true, "only": true, "own": true,
	"same": true, "just": true, "because": true, "as": true, "until": true,
	"while": true, "during": true, "before": true, "after": true, "above": true,
	"below": true, "between": true, "under": true, "again": true, "further": true,
	"once": true, "here": true, "there": true, "any": true, "can": true,
	"also": true, "may": true, "shall": true, "might": true, "must": true,
	"need": true, "very": true, "too": true, "these": true, "those": true,
}

// IsEnglish reports whether word is in the package's English
// stop-word baseline. The check is case-insensitive: word is
// lower-cased before lookup so callers can pass raw surface forms.
func IsEnglish(word string) bool {
	return stopWords[strings.ToLower(word)]
}

// EnglishSet returns a writable [Set] seeded with the package's
// English stop-word baseline. The returned Set is a fresh copy on
// every call — callers can [Set.Extend] it with domain words
// (product names, jargon) without affecting other consumers.
//
// This is the idiomatic way to opt into a stricter stop-word
// policy without forking the table: build a [Set] once at
// construction time and consult it via [Set.Contains] during
// tokenisation.
func EnglishSet() Set {
	s := make(Set, len(stopWords))
	for w := range stopWords {
		s[w] = struct{}{}
	}
	return s
}
