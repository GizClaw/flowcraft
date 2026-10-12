package tool

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/core/utils/bm25"
)

// SearchHit is one tool_search result.
type SearchHit struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Score       float64 `json:"score"`
}

// searchDoc is one searchable catalog entry.
type searchDoc struct {
	name        string
	description string
}

// defaultSearchLimit is used when tool_search omits limit.
const defaultSearchLimit = 8

// bm25Search ranks docs against query with the shared BM25 kernel.
// Documents are indexed by name, so the kernel's stable ranking breaks
// score ties by name. Limit <= 0 means the defaultSearchLimit.
func bm25Search(docs []searchDoc, query string, limit int) ([]SearchHit, error) {
	queryTerms := catalogTerms(query)
	if len(docs) == 0 || len(queryTerms) == 0 {
		return []SearchHit{}, nil
	}
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	// The kernel resolves ties by the order documents arrived in and the
	// catalog has always resolved them by name, so the index input is
	// sorted — this function's own slice, never the caller's.
	indexed := make([]bm25.TermDoc, 0, len(docs))
	descriptions := make(map[string]string, len(docs))
	for _, doc := range docs {
		description := strings.TrimSpace(doc.description)
		indexed = append(indexed, bm25.TermDoc{
			ID:   doc.name,
			Name: termFrequencies(catalogTerms(doc.name)),
			Text: termFrequencies(catalogTerms(description)),
		})
		descriptions[doc.name] = description
	}
	sort.Slice(indexed, func(i, j int) bool { return indexed[i].ID < indexed[j].ID })
	index, err := bm25.NewFromTerms(indexed)
	if err != nil {
		return nil, err
	}
	results := index.SearchTerms(queryTerms, limit)
	hits := make([]SearchHit, 0, len(results))
	for _, result := range results {
		hits = append(hits, SearchHit{
			Name:        result.ID,
			Description: descriptions[result.ID],
			Score:       result.Score,
		})
	}
	return hits, nil
}

// catalogTerms tokenizes value with the kernel's splitter and then drops
// single-rune ASCII tokens. A bare "a" or "1" is below the two-rune
// minimum the catalog has always used for ASCII words: it matches almost
// every definition, and with prefix matching on it expands to every term
// starting with that letter, so one filler word in a query is enough to
// pull unrelated definitions into the discovery pool. Single CJK
// characters stay — they are whole words there, and the kernel's bigrams
// need them to match a fragment.
func catalogTerms(value string) []string {
	tokens := bm25.Tokenize(value)
	kept := tokens[:0]
	for _, token := range tokens {
		if len(token) == 1 && token[0] < utf8.RuneSelf {
			continue
		}
		kept = append(kept, token)
	}
	return kept
}

// termFrequencies counts the terms of one field, the shape the kernel's
// TermDoc wants. Length is the sum of the counts, so a document's length
// counts exactly the terms the same function indexed.
func termFrequencies(tokens []string) map[string]int {
	counts := make(map[string]int, len(tokens))
	for _, token := range tokens {
		counts[token]++
	}
	return counts
}
