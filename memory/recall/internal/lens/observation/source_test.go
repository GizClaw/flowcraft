package observation

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/retrieval"
)

func TestSourceQueryCapsAndDeduplicatesObservationText(t *testing.T) {
	hits := []retrieval.Hit{
		{Doc: retrieval.Doc{ID: "obs-1", Content: "same text", Metadata: map[string]any{MetaObservationID: "obs-1"}}, Score: 0.9},
		{Doc: retrieval.Doc{ID: "obs-2", Content: " same   text ", Metadata: map[string]any{MetaObservationID: "obs-2"}}, Score: 0.8},
	}
	for i := 3; i <= 20; i++ {
		id := fmt.Sprintf("obs-%d", i)
		hits = append(hits, retrieval.Hit{
			Doc:   retrieval.Doc{ID: id, Content: fmt.Sprintf("same text unique %d", i), Metadata: map[string]any{MetaObservationID: id}},
			Score: 1 / float64(i),
		})
	}
	idx := fakeObservationIndex{hits: hits}
	src := NewSource(idx)

	got := src.Query(context.Background(), domain.QueryPlan{
		Intent: domain.QueryIntent{
			Text:  "same text",
			Scope: domain.Scope{RuntimeID: "rt", UserID: "u"},
		},
		SourceBudgets: map[string]int{src.Name(): 50},
		TotalCap:      30,
	})
	if got.Err != nil {
		t.Fatalf("Query returned error: %v", got.Err)
	}
	if len(got.Candidates) != observationSourceCandidateCap {
		t.Fatalf("got %d candidates, want cap %d", len(got.Candidates), observationSourceCandidateCap)
	}
	for _, cand := range got.Candidates {
		if cand.ID == "obs-2" {
			t.Fatalf("duplicate normalized observation text was not filtered: %+v", got.Candidates)
		}
	}
}

type fakeObservationIndex struct {
	retrieval.Index
	hits []retrieval.Hit
}

func (f fakeObservationIndex) Search(context.Context, string, retrieval.SearchRequest) (*retrieval.SearchResponse, error) {
	return &retrieval.SearchResponse{Hits: f.hits}, nil
}

func (f fakeObservationIndex) Capabilities() retrieval.Capabilities { return retrieval.Capabilities{} }
func (f fakeObservationIndex) Close() error                         { return nil }
