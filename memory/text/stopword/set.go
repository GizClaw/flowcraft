package stopword

import "strings"

// Set is a writable stop-word table. The underlying map is exposed
// so callers can range over it for diagnostics; mutation goes
// through [Set.Add], [Set.Extend] and [Set.Union] to keep
// case-folding consistent.
//
// Set is the primitive callers reach for when the package's
// baseline ([EnglishSet]) is the wrong policy: domain glossaries
// (product names, jargon), bilingual workloads (mix English +
// transliterated terms), and ablation experiments all want a
// scoped Set without forking the package's tables.
//
// Set is NOT safe for concurrent mutation. Construct it once at
// process startup and treat it as immutable afterwards.
type Set map[string]struct{}

// NewSet returns an empty Set. Seeded sets typically come from
// [EnglishSet] instead.
func NewSet() Set {
	return make(Set)
}

// Contains reports whether word is in the set. The check is
// case-insensitive — callers can pass raw surface forms.
func (s Set) Contains(word string) bool {
	if s == nil {
		return false
	}
	_, ok := s[strings.ToLower(word)]
	return ok
}

// Add inserts word into the set, lower-cased. Returns the receiver
// so calls can chain (s.Add("foo").Add("bar")).
func (s Set) Add(word string) Set {
	if word == "" {
		return s
	}
	s[strings.ToLower(word)] = struct{}{}
	return s
}

// Extend inserts each word into the set. Empty strings are
// skipped. Returns the receiver so calls can chain.
func (s Set) Extend(words ...string) Set {
	for _, w := range words {
		s.Add(w)
	}
	return s
}

// Union returns a new Set containing every entry from s and
// other. Neither input is mutated. Useful for layering a domain
// glossary on top of the package's baseline:
//
//	custom := stopword.EnglishSet().Union(domainSet)
func (s Set) Union(other Set) Set {
	out := make(Set, len(s)+len(other))
	for w := range s {
		out[w] = struct{}{}
	}
	for w := range other {
		out[w] = struct{}{}
	}
	return out
}

// Len reports the number of entries in the set.
func (s Set) Len() int { return len(s) }
