package observation

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/retrieval"
)

func TestProjectionForgetObservationsDeletesSpanDocs(t *testing.T) {
	idx := &recordingIndex{}
	proj := NewProjection(idx)
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	obs := domain.Observation{
		ID:       "obs-1",
		Scope:    scope,
		Kind:     domain.ObservationKindTurn,
		Text:     "Alice likes tea",
		SourceID: "msg-1",
		Spans: []domain.ObservationSpan{{
			ID:            "span-1",
			ObservationID: "obs-1",
			SourceID:      "msg-1",
			Kind:          domain.ObservationSpanKindQuote,
			Text:          "likes tea",
		}},
	}
	if err := proj.ProjectObservations(context.Background(), []domain.Observation{obs}); err != nil {
		t.Fatalf("ProjectObservations: %v", err)
	}
	if err := proj.ForgetObservations(context.Background(), scope, []string{"obs-1"}); err != nil {
		t.Fatalf("ForgetObservations: %v", err)
	}
	if !containsString(idx.deleted, "obs-1") || !containsString(idx.deleted, "span-1") {
		t.Fatalf("deleted ids = %v, want observation and span docs", idx.deleted)
	}
}

type recordingIndex struct {
	docs    []retrieval.Doc
	deleted []string
}

func (i *recordingIndex) Upsert(_ context.Context, _ string, docs []retrieval.Doc) error {
	i.docs = append(i.docs, docs...)
	return nil
}

func (i *recordingIndex) Delete(_ context.Context, _ string, ids []string) error {
	i.deleted = append(i.deleted, ids...)
	return nil
}

func (i *recordingIndex) Search(context.Context, string, retrieval.SearchRequest) (*retrieval.SearchResponse, error) {
	return &retrieval.SearchResponse{}, nil
}

func (i *recordingIndex) List(_ context.Context, _ string, req retrieval.ListRequest) (*retrieval.ListResponse, error) {
	var out []retrieval.Doc
	for _, doc := range i.docs {
		if retrieval.DocMatchesFilter(doc, req.Filter) {
			out = append(out, doc)
		}
	}
	return &retrieval.ListResponse{Items: out}, nil
}

func (i *recordingIndex) Capabilities() retrieval.Capabilities { return retrieval.Capabilities{} }
func (i *recordingIndex) Close() error                         { return nil }

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
