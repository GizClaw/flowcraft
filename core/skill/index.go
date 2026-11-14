package skill

import (
	"strings"

	"github.com/GizClaw/flowcraft/core/internal/search"
)

// Scored couples one ranked skill with its BM25 score, for search output and
// threshold tuning.
type Scored struct {
	Skill Metadata `json:"skill"`
	Score float64  `json:"score"`
}

// Index is an immutable BM25 index over a discovered skill set. It is built
// once per discovery pass: a caller that reloads skills builds a new Index
// rather than mutating this one, which is what keeps reads lock-free.
type Index struct {
	inner  *search.Index
	byPath map[string]Metadata
	byName map[string]Metadata
	// order is the catalog in discovery order, which is what an unfiltered
	// listing returns: a caller that wants "the first N skills" needs a stable
	// sequence, and the maps above cannot provide one.
	order []Metadata
}

// NewIndex indexes skill names and descriptions.
func NewIndex(skills []Metadata) (*Index, error) {
	docs := make([]search.Doc, 0, len(skills))
	byPath := make(map[string]Metadata, len(skills))
	byName := make(map[string]Metadata, len(skills))
	order := make([]Metadata, 0, len(skills))
	for _, skill := range skills {
		docs = append(docs, search.Doc{
			ID:   skill.Path,
			Name: skill.Name,
			Text: skill.Description,
		})
		if _, seen := byPath[skill.Path]; !seen {
			order = append(order, skill)
		}
		byPath[skill.Path] = skill
		if _, ok := byName[skill.Name]; !ok {
			byName[skill.Name] = skill
		}
	}
	inner, err := search.New(docs)
	if err != nil {
		return nil, err
	}
	return &Index{inner: inner, byPath: byPath, byName: byName, order: order}, nil
}

// Len reports how many skills are indexed.
func (ix *Index) Len() int { return len(ix.byPath) }

// All returns the catalog in discovery order. The slice is a copy: a caller
// cannot reorder what the index reports.
func (ix *Index) All() []Metadata {
	if ix == nil || len(ix.order) == 0 {
		return nil
	}
	out := make([]Metadata, len(ix.order))
	copy(out, ix.order)
	return out
}

// ByName resolves a skill by its declared name.
func (ix *Index) ByName(name string) (Metadata, bool) {
	skill, ok := ix.byName[name]
	return skill, ok
}

// RankScored returns the best matches for query, highest score first. A
// minScore <= 0 accepts any match; topN <= 0 means every match.
func (ix *Index) RankScored(query string, topN int, minScore float64) []Scored {
	if ix == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	if topN <= 0 {
		topN = ix.Len()
	}
	results := ix.inner.Search(query, topN)
	out := make([]Scored, 0, len(results))
	for _, result := range results {
		if minScore > 0 && result.Score < minScore {
			continue
		}
		if skill, ok := ix.byPath[result.ID]; ok {
			out = append(out, Scored{Skill: skill, Score: result.Score})
		}
	}
	return out
}

// Rank is RankScored without the scores. A nil result reads as "nothing to
// inject".
func (ix *Index) Rank(query string, topN int, minScore float64) []Metadata {
	scored := ix.RankScored(query, topN, minScore)
	if len(scored) == 0 {
		return nil
	}
	out := make([]Metadata, 0, len(scored))
	for _, sc := range scored {
		out = append(out, sc.Skill)
	}
	return out
}
