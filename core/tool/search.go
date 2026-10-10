package tool

import (
	"sort"
	"strings"

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
// Documents are sorted by name first, so the kernel's stable ranking
// breaks score ties by name and results stay deterministic. Limit <= 0
// means the defaultSearchLimit.
func bm25Search(docs []searchDoc, query string, limit int) ([]SearchHit, error) {
	if len(docs) == 0 || strings.TrimSpace(query) == "" {
		return []SearchHit{}, nil
	}
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].name < docs[j].name })
	indexed := make([]bm25.Doc, 0, len(docs))
	descriptions := make(map[string]string, len(docs))
	for _, doc := range docs {
		description := strings.TrimSpace(doc.description)
		indexed = append(indexed, bm25.Doc{ID: doc.name, Name: doc.name, Text: description})
		descriptions[doc.name] = description
	}
	index, err := bm25.New(indexed)
	if err != nil {
		return nil, err
	}
	results := index.Search(query, limit)
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
