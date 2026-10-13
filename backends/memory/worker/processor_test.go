package worker

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/lines/chat"
	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
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

// TestFactAddRequestSeparatesEventTimeFromIngestionTime pins the two-time
// contract: a dated fact keeps the deriver's event time and the commit's
// ingestion time side by side, while an undated fact falls back to the commit
// time for both and says so in metadata instead of passing the fallback off
// as a stated date.
func TestFactAddRequestSeparatesEventTimeFromIngestionTime(t *testing.T) {
	commit := msgsource.Commit{
		ID: "commit-1", Scope: corememory.Scope{RuntimeID: "memories"}, Version: 1,
		ConversationID: "conv-1",
		CreatedAt:      time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
	}
	fact := func(eventTime string) component.Artifact {
		metadata := corememory.Metadata{
			"canonical_hash": "hash", "source_digest": "digest", "transform_signature": "signature",
		}
		if eventTime != "" {
			metadata["event_time"] = eventTime
		}
		return component.Artifact{
			Kind: chat.KindFact, ID: "fact-1",
			Content: coremessage.NewTextContent("we talked about beverages"), Metadata: metadata,
		}
	}
	dated := factAddRequest(commit, fact("2023-05-08T13:56:00Z"), "policy-1")
	if want := time.Date(2023, 5, 8, 13, 56, 0, 0, time.UTC); !dated.EventTime.Equal(want) {
		t.Fatalf("dated event time = %v, want %v", dated.EventTime, want)
	}
	if !dated.IngestedAt.Equal(commit.CreatedAt) {
		t.Fatalf("dated ingested at = %v, want %v", dated.IngestedAt, commit.CreatedAt)
	}
	if got := dated.Metadata[factview.EventTimeInferredMetadataKey]; got != "" {
		t.Fatalf("dated fact carries the inferred mark %q", got)
	}
	undated := factAddRequest(commit, fact(""), "policy-1")
	if !undated.EventTime.Equal(commit.CreatedAt) || !undated.IngestedAt.Equal(commit.CreatedAt) {
		t.Fatalf("undated times = %v / %v, want the commit time for both", undated.EventTime, undated.IngestedAt)
	}
	if got := undated.Metadata[factview.EventTimeInferredMetadataKey]; got != "true" {
		t.Fatalf("undated fact inferred mark = %q, want true", got)
	}
}
