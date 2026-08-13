package tool

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// SearchHit is one tool_search result.
type SearchHit struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Score       float64 `json:"score"`
}

type searchDoc struct {
	name string
	text string
}

// defaultSearchLimit is used when tool_search omits limit.
const defaultSearchLimit = 8

// bm25Search ranks docs against query with Okapi BM25 (k1=1.2, b=0.75,
// smoothed IDF). Scores are computed on the fly: the tool catalog is
// bounded (hundreds to low thousands), so no persistent index is
// warranted. Ties break by name, keeping results deterministic.
func bm25Search(docs []searchDoc, query string, limit int) []SearchHit {
	queryTerms := tokenize(query)
	if len(queryTerms) == 0 || len(docs) == 0 {
		return []SearchHit{}
	}
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > len(docs) {
		limit = len(docs)
	}

	const k1, b = 1.2, 0.75
	termFreqs := make([]map[string]int, len(docs))
	df := make(map[string]int)
	avgDL := 0.0
	for i, doc := range docs {
		terms := tokenize(doc.text)
		freqs := make(map[string]int, len(terms))
		for _, term := range terms {
			freqs[term]++
		}
		termFreqs[i] = freqs
		for term := range freqs {
			df[term]++
		}
		avgDL += float64(len(terms))
	}
	n := float64(len(docs))
	if n > 0 {
		avgDL /= n
	}
	if avgDL <= 0 {
		avgDL = 1
	}

	idf := make(map[string]float64, len(df))
	for term, occurrences := range df {
		idf[term] = math.Log(1 + (n-float64(occurrences)+0.5)/(float64(occurrences)+0.5))
	}

	queryUnique := make(map[string]struct{}, len(queryTerms))
	for _, term := range queryTerms {
		queryUnique[term] = struct{}{}
	}

	type scored struct {
		index int
		score float64
	}
	scores := make([]scored, 0, len(docs))
	for i, freqs := range termFreqs {
		score := 0.0
		dl := 0
		for term := range freqs {
			dl += freqs[term]
		}
		for term := range queryUnique {
			tf := float64(freqs[term])
			if tf == 0 {
				continue
			}
			denominator := tf + k1*(1-b+b*float64(dl)/avgDL)
			score += idf[term] * (tf * (k1 + 1)) / denominator
		}
		if score > 0 {
			scores = append(scores, scored{index: i, score: score})
		}
	}
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].score != scores[j].score {
			return scores[i].score > scores[j].score
		}
		return docs[scores[i].index].name < docs[scores[j].index].name
	})
	if len(scores) > limit {
		scores = scores[:limit]
	}

	hits := make([]SearchHit, 0, len(scores))
	for _, s := range scores {
		doc := docs[s.index]
		description := ""
		if trimmed := strings.TrimSpace(strings.TrimPrefix(doc.text, doc.name)); trimmed != "" {
			description = trimmed
		}
		hits = append(hits, SearchHit{
			Name:        doc.name,
			Description: description,
			Score:       s.score,
		})
	}
	return hits
}

// tokenize lowercases and splits into Unicode letter/digit runs of at
// least two characters. Tool descriptions are short and provider-facing,
// so no stopword list is applied — every meaningful token contributes.
func tokenize(s string) []string {
	lower := strings.ToLower(s)
	runes := []rune(lower)
	var tokens []string
	start := -1
	for i, r := range runes {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 && i-start >= 2 {
			tokens = append(tokens, string(runes[start:i]))
		}
		start = -1
	}
	if start >= 0 && len(runes)-start >= 2 {
		tokens = append(tokens, string(runes[start:]))
	}
	return tokens
}
