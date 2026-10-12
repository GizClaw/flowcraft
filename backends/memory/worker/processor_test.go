package worker

import (
	"testing"
	"time"

	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// TestChatSourceAnchorsOnlyMessagesThatCarryObservedAt pins the injection
// contract: with the switch on, a record carrying the observed_at anchor is
// rendered as "[anchor] role: content" -- the shape the eval loader's date
// prefix uses -- while a record without one is rendered bare. The commit time
// is the storage time of the record, not an observation time, and must never
// stand in for one.
func TestChatSourceAnchorsOnlyMessagesThatCarryObservedAt(t *testing.T) {
	const anchor = "2023-05-08T13:56:00Z"
	commit := msgsource.Commit{
		ID: "commit-1", Scope: corememory.Scope{RuntimeID: "memories"}, Version: 1,
		ConversationID: "conv-1",
		CreatedAt:      time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
		Records: []msgsource.Record{
			{
				ID: "msg-1", ConversationID: "conv-1", Seq: 1,
				Message:  coremessage.NewTextMessage(coremessage.RoleUser, "we talked about beverages"),
				Metadata: corememory.Metadata{corememory.ObservedAtMetadataKey: " " + anchor + " "},
			},
			{
				ID: "msg-2", ConversationID: "conv-1", Seq: 2,
				Message:  coremessage.NewTextMessage(coremessage.RoleAssistant, "and about tea"),
				Metadata: corememory.Metadata{corememory.ObservedAtMetadataKey: "  "},
			},
		},
	}
	anchored := chatSource(commit, "policy-1", true).Content.Text()
	want := "[" + anchor + "] user: we talked about beverages\nassistant: and about tea"
	if anchored != want {
		t.Fatalf("anchored chat source = %q, want %q", anchored, want)
	}
	plain := chatSource(commit, "policy-1", false).Content.Text()
	wantPlain := "user: we talked about beverages\nassistant: and about tea"
	if plain != wantPlain {
		t.Fatalf("chat source with the switch off = %q, want %q", plain, wantPlain)
	}
}
