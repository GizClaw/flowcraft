package summary

import (
	"context"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	corebm25 "github.com/GizClaw/flowcraft/core/utils/bm25"
)

// Searcher exposes immutable summary records as a real retrieval lane.
type Searcher struct{ Store *SummaryStore }

var _ component.Searcher = (*Searcher)(nil)

func (searcher *Searcher) Search(ctx context.Context, request component.SearchRequest) ([]component.Candidate, error) {
	conversationID := request.Metadata["conversation_id"]
	if searcher == nil || searcher.Store == nil || conversationID == "" {
		return []component.Candidate{}, nil
	}
	// One locked read: the generation label and the records have to come from
	// the same manifest, and a publish between two calls would mix them.
	manifest, records, found, err := searcher.Store.ActiveSnapshot(ctx, request.Scope, conversationID)
	if err != nil {
		return nil, err
	}
	if !found {
		return []component.Candidate{}, nil
	}
	if generation := request.Metadata["generation_id"]; generation != "" && generation != manifest.GenerationID {
		return []component.Candidate{}, nil
	}
	query := corebm25.Tokenize(request.Query)
	result := make([]component.Candidate, 0, len(records))
	for _, record := range records {
		score := lexicalScore(query,
			corebm25.Tokenize(record.Text+" "+strings.Join(record.Topics, " ")))
		if score == 0 && len(query) > 0 {
			continue
		}
		result = append(result, component.Candidate{
			ID: record.ID, Lane: "summary", Name: "summary", Score: score,
			Source: record.SourceRefs[0],
			Address: component.CandidateAddress{
				Kind: corememory.ContextSummary, ConversationID: record.ConversationID, ItemID: record.ID,
			},
			Metadata: corememory.Metadata{"generation_id": manifest.GenerationID},
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Score != result[j].Score {
			return result[i].Score > result[j].Score
		}
		return result[i].ID < result[j].ID
	})
	if request.Limit > 0 && len(result) > request.Limit {
		result = result[:request.Limit]
	}
	return result, nil
}

// lexicalScore is the share of the query's terms the record mentions. Both
// sides are tokenized with the shared kernel (core/utils/bm25), the same
// splitter the projection lanes match with: ASCII word runs, and CJK as
// single characters plus adjacent bigrams, so a Chinese query matches a
// record that mentions it instead of tokenizing to one unmatchable run.
func lexicalScore(query, text []string) float64 {
	if len(query) == 0 {
		return 1
	}
	set := make(map[string]struct{}, len(text))
	for _, term := range text {
		set[term] = struct{}{}
	}
	matched := 0
	for _, term := range query {
		if _, ok := set[term]; ok {
			matched++
		}
	}
	return float64(matched) / float64(len(query))
}
