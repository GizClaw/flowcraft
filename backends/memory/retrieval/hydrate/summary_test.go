package hydrate

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	messagesource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	summaryview "github.com/GizClaw/flowcraft/backends/memory/views/summary"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

func TestSummarySearchHydrateHintAndStrictExpansion(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	ws := newTestWorkspace(t)
	messages := newMessageStore(t, ws)
	summaries := newSummaryStore(t, ws)
	records, err := messages.Append(ctx, messagesource.AppendRequest{
		Scope: scope, ConversationID: "conversation", IdempotencyKey: "turn",
		Messages: []coremessage.Message{coremessage.NewTextMessage(coremessage.RoleUser, "architecture decision")},
	})
	if err != nil {
		t.Fatal(err)
	}
	source := corememory.SourceRef{
		Kind: corememory.SourceMessage, ID: "conversation/" + records[0].ID, Revision: "1",
	}
	record, err := summaries.Add(ctx, summaryview.AddRequest{
		ID: "summary", Scope: scope, ConversationID: "conversation", Level: summaryview.L1,
		Text: "architecture decision", Content: textContent("architecture decision"),
		Topics: []string{"architecture"}, InputIDs: []string{"fact"},
		SourceRefs:    []corememory.SourceRef{source},
		CoverageRange: summaryview.CoverageRange{StartSeq: 1, EndSeq: 1},
		SourceDigest:  "digest", TransformSignature: "compact-v1", GenerationID: "generation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := summaries.PublishActive(ctx, summaryview.Manifest{
		Scope: scope, ConversationID: "conversation", GenerationID: "generation",
		RecordIDs: []string{record.ID}, CoverageRange: record.CoverageRange, FrontierDigest: "frontier",
	}); err != nil {
		t.Fatal(err)
	}
	searcher := &summaryview.Searcher{Store: summaries}
	candidates, err := searcher.Search(ctx, component.SearchRequest{
		Scope: scope, Query: "architecture", Limit: 3,
		Metadata: corememory.Metadata{"conversation_id": "conversation", "generation_id": "generation"},
	})
	if err != nil || len(candidates) != 1 || candidates[0].ID != record.ID {
		t.Fatalf("candidates=%#v err=%v", candidates, err)
	}
	hydrator := &Composite{Messages: messages, Summaries: summaries}
	item, err := hydrator.Hydrate(ctx, scope, candidates[0])
	if err != nil || item.Kind != corememory.ContextSummary ||
		item.Content.Text() != "architecture decision" {
		t.Fatalf("item=%#v err=%v", item, err)
	}
	candidates[0].Source.Revision = "0"
	if _, err := hydrator.Hydrate(ctx, scope, candidates[0]); err == nil {
		t.Fatal("stale summary candidate hydrated")
	}
}
