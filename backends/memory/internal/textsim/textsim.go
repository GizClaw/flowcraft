// Package textsim is the token similarity the memory module's duplicate rules
// share.
//
// Tokens are the shared kernel's (core/utils/bm25.Tokenize), the same splitter
// the retrieval lanes match with, so "similar" means "the lexical lanes would
// weigh these two as one content" and the rules that merge or supersede facts
// agree with the lanes that retrieve them.
package textsim

import corebm25 "github.com/GizClaw/flowcraft/core/utils/bm25"

// Set returns the token set of value.
func Set(value string) map[string]struct{} {
	tokens := corebm25.Tokenize(value)
	set := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		set[token] = struct{}{}
	}
	return set
}

// Jaccard returns the token Jaccard similarity of two texts: the share of
// tokens they have in common. Two texts that share no token, or either of
// which has no token at all, are 0 rather than 1.
func Jaccard(left, right string) float64 {
	return Similarity(Set(left), Set(right))
}

// Similarity is Jaccard over two token sets already in hand.
func Similarity(left, right map[string]struct{}) float64 {
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	shared := 0
	for token := range left {
		if _, ok := right[token]; ok {
			shared++
		}
	}
	union := len(left) + len(right) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}

// Containment returns the overlap coefficient of two texts: the share of the
// shorter text's tokens that the longer one also carries. It answers a different
// question than Jaccard does, and the one a duplicate rule has to ask: is the
// shorter text already said by the longer? A restatement that adds a trailing
// clause is not very similar by Jaccard -- the added tokens grow the union -- but
// it is fully contained, and it is still one claim spending two slots.
func Containment(left, right string) float64 {
	return ContainmentOf(Set(left), Set(right))
}

// ContainmentOf is Containment over two token sets already in hand.
func ContainmentOf(left, right map[string]struct{}) float64 {
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	smaller, larger := left, right
	if len(right) < len(left) {
		smaller, larger = right, left
	}
	shared := 0
	for token := range smaller {
		if _, ok := larger[token]; ok {
			shared++
		}
	}
	return float64(shared) / float64(len(smaller))
}
