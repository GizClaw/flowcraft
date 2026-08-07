package retrieval

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/memory/component"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

func TestLaneBackendSearchMapsThresholdTopKAndPayload(t *testing.T) {
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
	lane := &fakeLane{searcher: func(request component.SearchRequest) []component.Candidate {
		if request.Scope != scope {
			t.Fatalf("scope = %v, want %v", request.Scope, scope)
		}
		return []component.Candidate{
			{ID: "low", Score: 0.2, Lane: "fake", Address: component.CandidateAddress{Kind: sdkmemory.ContextFact, ConversationID: "c", ItemID: "low"}},
			{ID: "high", Score: 0.9, Lane: "fake", Address: component.CandidateAddress{Kind: sdkmemory.ContextFact, ConversationID: "c", ItemID: "high"}},
			{ID: "mid", Score: 0.5, Lane: "fake", Address: component.CandidateAddress{Kind: sdkmemory.ContextFact, ConversationID: "c", ItemID: "mid"}},
		}
	}}
	backend := NewLaneBackend(lane)
	hits, err := backend.Search(context.Background(), "facts", SearchQuery{
		Scope: scope, Text: "query", TopK: 2, Threshold: 0.3,
		Filters: map[string]any{"conversation_id": "c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].ID != "high" || hits[1].ID != "mid" {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].Score != 0.9 || hits[1].Score != 0.5 {
		t.Fatalf("scores = %+v", hits)
	}
	address, ok := hits[0].Payload["address"].(component.CandidateAddress)
	if !ok || address.ItemID != "high" {
		t.Fatalf("payload address = %#v, %v", hits[0].Payload["address"], ok)
	}
	if lane.lastRequest.Metadata["conversation_id"] != "c" {
		t.Fatalf("filters did not reach the lane: %#v", lane.lastRequest.Metadata)
	}
}

func TestLaneBackendWritesTranslateDocumentsToArtifacts(t *testing.T) {
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
	lane := &fakeLane{}
	backend := NewLaneBackend(lane)
	doc := Document{
		ID:   "fact-1",
		Text: "stable fact",
		Payload: map[string]any{
			"kind": "fact",
			"sources": []sdkmemory.SourceRef{{
				Kind: sdkmemory.SourceMessage, ID: "conversation/message", Revision: "1",
			}},
			"metadata": sdkmemory.Metadata{"conversation_id": "c"},
		},
	}
	if err := backend.Upsert(context.Background(), "facts", scope, doc.ID, doc); err != nil {
		t.Fatal(err)
	}
	if lane.lastDelta.Scope != scope || lane.lastDelta.Projection != "facts" ||
		len(lane.lastDelta.Upserts) != 1 || lane.lastDelta.Upserts[0].ID != doc.ID ||
		lane.lastDelta.Upserts[0].Content.Text() != doc.Text ||
		lane.lastDelta.Upserts[0].Metadata["conversation_id"] != "c" {
		t.Fatalf("delta = %+v", lane.lastDelta)
	}

	docs := []Document{
		{ID: "a", Text: "a", Payload: map[string]any{"kind": "fact", "sources": doc.Payload["sources"]}},
		{ID: "b", Text: "b", Payload: map[string]any{"kind": "fact", "sources": doc.Payload["sources"]}},
	}
	if err := backend.ReplaceAll(context.Background(), "facts", scope, docs); err != nil {
		t.Fatal(err)
	}
	if lane.lastRebuild.Scope != scope || lane.lastRebuild.Projection != "facts" ||
		len(lane.lastRebuild.Artifacts) != 2 {
		t.Fatalf("rebuild = %+v", lane.lastRebuild)
	}
	if err := backend.Delete(context.Background(), "facts", scope, "a"); err != nil {
		t.Fatal(err)
	}
	if lane.lastDelta.Scope != scope || lane.lastDelta.Projection != "facts" ||
		!reflect.DeepEqual(lane.lastDelta.DeleteIDs, []string{"a"}) {
		t.Fatalf("delete delta = %+v", lane.lastDelta)
	}
	if err := backend.Upsert(context.Background(), "facts", scope, "x", Document{ID: "x", Payload: map[string]any{}}); err == nil {
		t.Fatal("missing artifact kind accepted")
	}
}

type fakeLane struct {
	searcher    func(component.SearchRequest) []component.Candidate
	lastRequest component.SearchRequest
	lastDelta   component.ProjectionDelta
	lastRebuild component.ProjectionRequest
}

func (lane *fakeLane) Search(ctx context.Context, request component.SearchRequest) ([]component.Candidate, error) {
	lane.lastRequest = request
	if lane.searcher == nil {
		return []component.Candidate{}, nil
	}
	return lane.searcher(request), nil
}

func (lane *fakeLane) ApplyDelta(ctx context.Context, delta component.ProjectionDelta) error {
	lane.lastDelta = delta
	return nil
}

func (lane *fakeLane) FullRebuild(ctx context.Context, request component.ProjectionRequest) error {
	lane.lastRebuild = request
	return nil
}

func (lane *fakeLane) Rebuild(context.Context, component.ProjectionRequest) error {
	return errors.New("not used")
}
