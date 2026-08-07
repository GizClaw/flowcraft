package summary

import (
	"context"
	"sort"
	"strings"
	"unicode"

	"github.com/GizClaw/flowcraft/memory/component"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

// Searcher exposes immutable summary records as a real retrieval lane.
type Searcher struct{ Store *SummaryStore }

var _ component.Searcher = (*Searcher)(nil)

func (searcher *Searcher) Search(ctx context.Context, request component.SearchRequest) ([]component.Candidate, error) {
	conversationID := request.Metadata["conversation_id"]
	if searcher == nil || searcher.Store == nil || conversationID == "" {
		return []component.Candidate{}, nil
	}
	manifest, found, err := searcher.Store.LoadActive(ctx, request.Scope, conversationID)
	if err != nil {
		return nil, err
	}
	if !found {
		return []component.Candidate{}, nil
	}
	records, err := searcher.Store.ListActive(ctx, request.Scope, conversationID,
		ListOptions{GenerationID: request.Metadata["generation_id"]})
	if err != nil {
		return nil, err
	}
	query := terms(request.Query)
	result := make([]component.Candidate, 0, len(records))
	for _, record := range records {
		score := lexicalScore(query, terms(record.Text+" "+strings.Join(record.Topics, " ")))
		if score == 0 && len(query) > 0 {
			continue
		}
		result = append(result, component.Candidate{
			ID: record.ID, Lane: "summary", Name: "summary", Score: score,
			Source: record.SourceRefs[0],
			Address: component.CandidateAddress{
				Kind: sdkmemory.ContextSummary, ConversationID: record.ConversationID, ItemID: record.ID,
			},
			Metadata: sdkmemory.Metadata{"generation_id": manifest.GenerationID},
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

func terms(value string) []string {
	return strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

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
