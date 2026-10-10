package skill

import (
	"regexp"
	"strings"
)

// Scored couples one ranked skill with its BM25 score, for
// observability (search output, telemetry, threshold tuning).
type Scored struct {
	Skill Metadata
	Score float64
}

// mentionRe matches $name mentions that stand alone (start of text or
// preceded by a non-word character), so "$50" inside "$500" or
// "a$skill" mid-word is not treated as a mention.
var mentionRe = regexp.MustCompile(`(?:^|[^a-z0-9_])[$]([a-z0-9]+(?:-[a-z0-9]+)*)`)

// Mentioned extracts explicit $name mentions from text and resolves
// them to skills in mention order, keeping the first mention of each
// skill.
func (s *Service) Mentioned(text string) []Metadata {
	var out []Metadata
	seen := map[string]bool{}
	for _, m := range mentionRe.FindAllStringSubmatch(text, -1) {
		sk, ok := s.ByName(m[1])
		if !ok || seen[sk.Path] {
			continue
		}
		seen[sk.Path] = true
		out = append(out, sk)
	}
	return out
}

// Rank returns the topN skills for query scoring at least minScore,
// sorted by BM25 score descending. A minScore <= 0 accepts any match,
// and a topN <= 0 falls back to [Service.TopN]. An empty query — or
// one that matches nothing — returns nil, so a nil result reads as
// "nothing to inject".
func (s *Service) Rank(query string, topN int, minScore float64) []Metadata {
	scored := s.RankScored(query, topN, minScore)
	if len(scored) == 0 {
		return nil
	}
	out := make([]Metadata, 0, len(scored))
	for _, sc := range scored {
		out = append(out, sc.Skill)
	}
	return out
}

// RankScored is Rank with scores attached.
func (s *Service) RankScored(query string, topN int, minScore float64) []Scored {
	snap := s.snapshot.Load()
	if snap.index == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	limit := topN
	if limit <= 0 {
		limit = s.opts.TopN
	}
	results := snap.index.Search(query, limit)
	out := make([]Scored, 0, len(results))
	for _, r := range results {
		if minScore > 0 && r.Score < minScore {
			continue
		}
		if sk, ok := snap.byPath[r.ID]; ok {
			out = append(out, Scored{Skill: sk, Score: r.Score})
		}
	}
	return out
}
