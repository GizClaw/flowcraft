package retrieval

import (
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	projectionstore "github.com/GizClaw/flowcraft/backends/memory/internal/projection"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// TestMatchesRequestRechecksFusedCandidatesByAxis is the post-fusion half of
// the strict rule: a candidate that reached fusion is judged by the selector
// the lanes used, so a request that named one axis cannot serve the other's
// records through a lane that ignored it. Candidates without an explicit
// address fall back to the conversation they report, which is all a locator
// envelope leaves to check.
func TestMatchesRequestRechecksFusedCandidatesByAxis(t *testing.T) {
	chat := component.Candidate{Address: component.CandidateAddress{
		Kind: corememory.ContextFact, ConversationID: "c1",
	}}
	document := component.Candidate{Address: component.CandidateAddress{
		Kind: corememory.ContextDocumentChunk, DatasetID: "d1", DocumentID: "doc",
	}}
	locator := component.Candidate{Address: component.CandidateAddress{ConversationID: "c1"}}
	otherLocator := component.Candidate{Address: component.CandidateAddress{ConversationID: "c2"}}

	for _, test := range []struct {
		name     string
		metadata corememory.Metadata
		request  corememory.ContextRequest
		want     map[string]bool
	}{
		{
			name:     "conversation",
			metadata: corememory.Metadata{"conversation_id": "c1"},
			request:  corememory.ContextRequest{ConversationID: "c1"},
			want:     map[string]bool{"chat": true, "document": false, "locator": true, "other": false},
		},
		{
			name:     "datasets",
			metadata: corememory.Metadata{"dataset_ids": `["d1"]`},
			request:  corememory.ContextRequest{DatasetIDs: []string{"d1"}},
			want:     map[string]bool{"chat": false, "document": true, "locator": true, "other": true},
		},
		{
			name:     "both",
			metadata: corememory.Metadata{"conversation_id": "c1", "dataset_ids": `["d1"]`},
			request:  corememory.ContextRequest{ConversationID: "c1", DatasetIDs: []string{"d1"}},
			want:     map[string]bool{"chat": true, "document": true, "locator": true, "other": false},
		},
		{
			name:     "global",
			metadata: nil,
			request:  corememory.ContextRequest{},
			want:     map[string]bool{"chat": true, "document": true, "locator": true, "other": true},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			selector, err := projectionstore.ParseSelector(test.metadata)
			if err != nil {
				t.Fatal(err)
			}
			for name, want := range test.want {
				candidate := map[string]component.Candidate{
					"chat": chat, "document": document, "locator": locator, "other": otherLocator,
				}[name]
				if got := matchesRequest(candidate, test.request, selector); got != want {
					t.Errorf("%s candidate: matches = %v, want %v", name, got, want)
				}
			}
		})
	}
}
