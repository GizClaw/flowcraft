package hydrate

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/component"
	messagesource "github.com/GizClaw/flowcraft/memory/sources/message"
	summaryview "github.com/GizClaw/flowcraft/memory/views/summary"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestSummarySearchHydrateHintAndStrictExpansion(t *testing.T) {
	ctx := context.Background()
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
	ws := workspace.NewMemWorkspace()
	messages := newMessageStore(t, ws)
	summaries := newSummaryStore(t, ws)
	records, err := messages.Append(ctx, messagesource.AppendRequest{
		Scope: scope, ConversationID: "conversation", IdempotencyKey: "turn",
		Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, "architecture decision")},
	})
	if err != nil {
		t.Fatal(err)
	}
	source := sdkmemory.SourceRef{
		Kind: sdkmemory.SourceMessage, ID: "conversation/" + records[0].ID, Revision: "1",
	}
	record, err := summaries.Add(ctx, summaryview.AddRequest{
		ID: "summary", Scope: scope, ConversationID: "conversation", Level: summaryview.L1,
		Text: "architecture decision", Content: textContent("architecture decision"),
		Topics: []string{"architecture"}, InputIDs: []string{"fact"},
		SourceRefs:    []sdkmemory.SourceRef{source},
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
		Metadata: sdkmemory.Metadata{"conversation_id": "conversation", "generation_id": "generation"},
	})
	if err != nil || len(candidates) != 1 || candidates[0].ID != record.ID {
		t.Fatalf("candidates=%#v err=%v", candidates, err)
	}
	hydrator := &Composite{Messages: messages, Summaries: summaries}
	item, err := hydrator.Hydrate(ctx, scope, candidates[0])
	if err != nil || item.Kind != sdkmemory.ContextSummary || item.Hint == nil ||
		item.Hint.Range.StartSequence != 1 || item.Content.Text() != "architecture decision" {
		t.Fatalf("item=%#v err=%v", item, err)
	}
	expanded, err := hydrator.Expand(ctx, scope, *item.Hint)
	if err != nil || len(expanded) != 1 || expanded[0].Content.Text() != "architecture decision" {
		t.Fatalf("expanded=%#v err=%v", expanded, err)
	}
	stale := *item.Hint
	stale.SourceRefs = append([]sdkmemory.SourceRef(nil), stale.SourceRefs...)
	stale.SourceRefs[0].Revision = "0"
	if _, err := hydrator.Expand(ctx, scope, stale); err == nil {
		t.Fatal("stale source expanded")
	}
	candidates[0].Source.Revision = "0"
	if _, err := hydrator.Hydrate(ctx, scope, candidates[0]); err == nil {
		t.Fatal("stale summary candidate hydrated")
	}
}
