package engine

import (
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/search"
	"github.com/GizClaw/flowcraft/core/tool"
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

// searchIndex is a built BM25 index over one catalog, plus the descriptions a
// hit has to echo back. Building it is the expensive half of tool_search —
// every name and description is tokenized and every posting list is built —
// while answering a query is a lookup per query term, so the index is worth
// keeping while the catalog stands still.
//
// A built index is immutable, which is what makes it safe to share: the kernel
// keeps its scoring scratch space on the caller's stack, not in the index.
type searchIndex struct {
	index        *search.Index
	descriptions map[string]string
}

// searchDocs lowers the searchable definitions of a catalog onto the kernel's
// input. Documents are indexed by name, so the kernel's stable ranking breaks
// score ties by name.
func searchDocs(defs []message.ToolDefinition) []searchDoc {
	docs := make([]searchDoc, 0, len(defs))
	for _, def := range defs {
		docs = append(docs, searchDoc{name: def.Name, description: def.Description})
	}
	return docs
}

// buildSearchIndex tokenizes every doc once and builds the BM25 index. The
// input is sorted because the kernel resolves ties by arrival order and the
// catalog has always resolved them by name — this function's own slice, never
// the caller's.
func buildSearchIndex(docs []searchDoc) (*searchIndex, error) {
	indexed := make([]search.TermDoc, 0, len(docs))
	descriptions := make(map[string]string, len(docs))
	for _, doc := range docs {
		description := strings.TrimSpace(doc.description)
		indexed = append(indexed, search.TermDoc{
			ID:   doc.name,
			Name: termFrequencies(catalogTerms(doc.name)),
			Text: termFrequencies(catalogTerms(description)),
		})
		descriptions[doc.name] = description
	}
	sort.Slice(indexed, func(i, j int) bool { return indexed[i].ID < indexed[j].ID })
	index, err := search.NewFromTerms(indexed)
	if err != nil {
		return nil, err
	}
	return &searchIndex{index: index, descriptions: descriptions}, nil
}

// search ranks the built index against a query. Limit <= 0 means the
// defaultSearchLimit.
func (ix *searchIndex) search(query string, limit int) []SearchHit {
	queryTerms := catalogTerms(query)
	if ix == nil || len(queryTerms) == 0 || ix.index == nil {
		return []SearchHit{}
	}
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	results := ix.index.SearchTerms(queryTerms, limit)
	hits := make([]SearchHit, 0, len(results))
	for _, result := range results {
		hits = append(hits, SearchHit{
			Name:        result.ID,
			Description: ix.descriptions[result.ID],
			Score:       result.Score,
		})
	}
	return hits
}

// definitionEpoch is the optional capability a catalog offers when it can say
// whether its definition set changed. A catalog without it is still searchable;
// its index is just rebuilt per call, which is what every call paid before.
type definitionEpoch interface {
	DefinitionsEpoch() uint64
}

// searchIndexCache keeps one built index alive across tool_search calls and
// across the catalog views an assembly hands out. The index is derived from the
// catalog, which changes far less often than the tool is called: a deployment
// with a few hundred tools rebuilt a megabyte of postings on every search.
//
// It lives on the [Assembly] rather than on a view because a view is per run —
// one index per deployment, not one per conversation.
type searchIndexCache struct {
	mu    sync.Mutex
	epoch uint64
	built *searchIndex
	keyed bool
}

// get returns the cached index when it still describes catalog, and otherwise
// builds one from build. build runs under the lock so concurrent searches share
// one rebuild rather than racing to duplicate it.
func (c *searchIndexCache) get(catalog tool.Catalog, build func() []searchDoc) (*searchIndex, error) {
	epoch, keyed := uint64(0), false
	if aware, ok := catalog.(definitionEpoch); ok {
		epoch, keyed = aware.DefinitionsEpoch(), true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if keyed && c.keyed && c.built != nil && c.epoch == epoch {
		return c.built, nil
	}
	index, err := buildSearchIndex(build())
	if err != nil {
		return nil, err
	}
	if keyed {
		c.built, c.epoch, c.keyed = index, epoch, true
	}
	return index, nil
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
	tokens := search.Tokenize(value)
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
