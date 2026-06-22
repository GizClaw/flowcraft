package summary

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/derive"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewrecent "github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/model"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestBufferSummarizerReturnsNilWithoutEvictedMessages(t *testing.T) {
	ctx := context.Background()
	summarizer := BufferSummarizer{}
	input := summaryInput(nil, nil, derive.SummaryPolicy{PreserveRecentMessages: 2})

	nodes, err := summarizer.Summarize(ctx, input)
	if err != nil {
		t.Fatalf("Summarize empty window error = %v", err)
	}
	if nodes != nil {
		t.Fatalf("Summarize empty window = %+v, want nil", nodes)
	}

	input.Window = summaryWindow([]sourcemessage.Message{
		message("msg-1", 1, model.RoleUser, "keep one", testTime(1)),
		message("msg-2", 2, model.RoleAssistant, "keep two", testTime(2)),
	})
	nodes, err = summarizer.Summarize(ctx, input)
	if err != nil {
		t.Fatalf("Summarize preserved-only window error = %v", err)
	}
	if nodes != nil {
		t.Fatalf("Summarize preserved-only window = %+v, want nil", nodes)
	}
}

func TestBufferSummarizerGeneratesLevelZeroSummaryForEvictedMessages(t *testing.T) {
	ctx := context.Background()
	msgs := []sourcemessage.Message{
		message("msg-1", 1, model.RoleUser, "Ada likes tea.", testTime(1)),
		message("msg-2", 2, model.RoleAssistant, "Noted. I will remember tea.", testTime(2)),
		message("msg-3", 3, model.RoleUser, "Leave this raw.", testTime(3)),
	}
	input := summaryInput(msgs, nil, derive.SummaryPolicy{
		MaxRawMessages:         1,
		PreserveRecentMessages: 1,
		MaxSummaryBytes:        2048,
	})

	nodes, err := (BufferSummarizer{}).Summarize(ctx, input)
	if err != nil {
		t.Fatalf("Summarize error = %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("Summarize nodes len = %d, want 1", len(nodes))
	}
	node := nodes[0]
	if node.Level != 0 {
		t.Fatalf("Level = %d, want 0", node.Level)
	}
	if len(node.ParentIDs) != 0 {
		t.Fatalf("ParentIDs = %+v, want empty", node.ParentIDs)
	}
	if len(node.SourceRefs) != 2 || len(node.Signature.SourceRevisions) != 2 {
		t.Fatalf("source refs/revisions len = %d/%d, want 2/2", len(node.SourceRefs), len(node.Signature.SourceRevisions))
	}
	if node.Signature.TransformSignature != "summary_buffer:v1:max_raw=1:preserve=1:max_bytes=2048" {
		t.Fatalf("TransformSignature = %q", node.Signature.TransformSignature)
	}
	if node.Metadata["algorithm"] != "summary_buffer" || node.Metadata["folded_message_count"] != 2 || node.Metadata["preserve_recent_messages"] != 1 {
		t.Fatalf("Metadata = %+v, want summary_buffer policy metadata", node.Metadata)
	}
	if node.Signature.SourceRevisions[0].Revision != "1" || node.Signature.SourceRevisions[0].ContentHash == "" || !node.Signature.SourceRevisions[0].ObservedAt.Equal(testTime(1)) {
		t.Fatalf("first source revision = %+v, want message seq/content hash/observed_at", node.Signature.SourceRevisions[0])
	}
	if !strings.Contains(node.Summary, "- user: Ada likes tea.") || strings.Contains(node.Summary, "Leave this raw") {
		t.Fatalf("Summary = %q, want folded messages only", node.Summary)
	}
	assertValidSummaryNode(t, node)

	again, err := (BufferSummarizer{}).Summarize(ctx, input)
	if err != nil {
		t.Fatalf("Summarize again error = %v", err)
	}
	if len(again) != 1 || again[0].ID != node.ID {
		t.Fatalf("stable ID = %+v then %+v, want same", node.ID, again)
	}
}

func TestBufferSummarizerChainsPreviousSummaryWithNewEvictedMessages(t *testing.T) {
	ctx := context.Background()
	previousMsg := message("msg-1", 1, model.RoleUser, "Previous evidence.", testTime(1))
	previous := summaryNode("previous", []sourcemessage.Message{previousMsg}, "Ada likes tea.", 2, testTime(10))
	msgs := []sourcemessage.Message{
		message("msg-2", 2, model.RoleUser, "Ada now prefers oolong.", testTime(2)),
		message("msg-3", 3, model.RoleAssistant, "Preference updated.", testTime(3)),
		message("msg-4", 4, model.RoleUser, "Keep this as raw.", testTime(4)),
	}
	input := summaryInput(msgs, []viewrecent.SummaryNode{previous}, derive.SummaryPolicy{MaxRawMessages: 1, PreserveRecentMessages: 1})

	nodes, err := (BufferSummarizer{}).Summarize(ctx, input)
	if err != nil {
		t.Fatalf("Summarize error = %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("Summarize nodes len = %d, want 1", len(nodes))
	}
	node := nodes[0]
	if len(node.ParentIDs) != 1 || node.ParentIDs[0] != previous.ID {
		t.Fatalf("ParentIDs = %+v, want previous %q", node.ParentIDs, previous.ID)
	}
	if !strings.Contains(node.Summary, "Previous summary:\nAda likes tea.") || !strings.Contains(node.Summary, "- user: Ada now prefers oolong.") {
		t.Fatalf("Summary = %q, want previous summary and new facts", node.Summary)
	}
	if len(node.SourceRefs) != 3 || len(node.Signature.SourceRevisions) != 3 {
		t.Fatalf("source refs/revisions len = %d/%d, want previous plus two folded messages", len(node.SourceRefs), len(node.Signature.SourceRevisions))
	}
	if got := node.Metadata["previous_summary_id"]; got != string(previous.ID) {
		t.Fatalf("previous_summary_id = %#v, want %q", got, previous.ID)
	}
	assertValidSummaryNode(t, node)
}

func TestBufferSummarizerReturnsNilWithPreviousButNoNewEvictedMessages(t *testing.T) {
	ctx := context.Background()
	previousMsg := message("msg-1", 1, model.RoleUser, "Previous evidence.", testTime(1))
	previous := summaryNode("previous", []sourcemessage.Message{previousMsg}, "Ada likes tea.", 2, testTime(10))
	msgs := []sourcemessage.Message{
		message("msg-2", 2, model.RoleUser, "Keep raw.", testTime(2)),
		message("msg-3", 3, model.RoleAssistant, "Also raw.", testTime(3)),
	}
	input := summaryInput(msgs, []viewrecent.SummaryNode{previous}, derive.SummaryPolicy{PreserveRecentMessages: 4})

	nodes, err := (BufferSummarizer{}).Summarize(ctx, input)
	if err != nil {
		t.Fatalf("Summarize error = %v", err)
	}
	if nodes != nil {
		t.Fatalf("Summarize = %+v, want nil when there are no new evicted messages", nodes)
	}
}

func TestBufferSummarizerSkipsMessagesCoveredByPreviousSummary(t *testing.T) {
	ctx := context.Background()
	covered := message("msg-1", 1, model.RoleUser, "Already summarized.", testTime(1))
	previous := summaryNode("previous", []sourcemessage.Message{covered}, "Already summarized.", 0, testTime(10))
	msgs := []sourcemessage.Message{
		covered,
		message("msg-2", 2, model.RoleAssistant, "Keep raw.", testTime(2)),
	}
	input := summaryInput(msgs, []viewrecent.SummaryNode{previous}, derive.SummaryPolicy{MaxRawMessages: 1, PreserveRecentMessages: 1})

	nodes, err := (BufferSummarizer{}).Summarize(ctx, input)
	if err != nil {
		t.Fatalf("Summarize error = %v", err)
	}
	if nodes != nil {
		t.Fatalf("Summarize = %+v, want nil when evicted messages are already covered", nodes)
	}
}

func TestBufferSummarizerTruncatesSummaryDeterministically(t *testing.T) {
	ctx := context.Background()
	msgs := []sourcemessage.Message{
		message("msg-1", 1, model.RoleUser, strings.Repeat("first ", 20), testTime(1)),
		message("msg-2", 2, model.RoleAssistant, strings.Repeat("second ", 20), testTime(2)),
		message("msg-3", 3, model.RoleUser, "preserved", testTime(3)),
	}
	input := summaryInput(msgs, nil, derive.SummaryPolicy{MaxRawMessages: 1, PreserveRecentMessages: 1, MaxSummaryBytes: 96})

	nodes, err := (BufferSummarizer{}).Summarize(ctx, input)
	if err != nil {
		t.Fatalf("Summarize error = %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("Summarize nodes len = %d, want 1", len(nodes))
	}
	if len(nodes[0].Summary) > 96 {
		t.Fatalf("Summary len = %d, want <= 96: %q", len(nodes[0].Summary), nodes[0].Summary)
	}
	if !strings.Contains(nodes[0].Summary, "... [truncated] ...") {
		t.Fatalf("Summary = %q, want truncation marker", nodes[0].Summary)
	}
	assertValidSummaryNode(t, nodes[0])
}

func summaryInput(messages []sourcemessage.Message, current []viewrecent.SummaryNode, policy derive.SummaryPolicy) derive.SummaryInput {
	return derive.SummaryInput{
		View:    views.Descriptor{ID: "summary-dag"},
		Scope:   testScope(),
		Window:  summaryWindow(messages),
		Current: current,
		Policy:  policy,
	}
}

func summaryWindow(messages []sourcemessage.Message) viewrecent.WindowResult {
	refs := make([]views.SourceRef, 0, len(messages))
	for _, msg := range messages {
		refs = append(refs, messageRef(msg))
	}
	return viewrecent.WindowResult{
		Descriptor: views.Descriptor{ID: "recent-window"},
		Messages:   messages,
		SourceRefs: refs,
	}
}

func summaryNode(id string, messages []sourcemessage.Message, text string, level int, updatedAt time.Time) viewrecent.SummaryNode {
	refs := make([]views.SourceRef, 0, len(messages))
	revisions := make([]views.SourceRevision, 0, len(messages))
	for _, msg := range messages {
		ref := messageRef(msg)
		refs = append(refs, ref)
		revisions = append(revisions, messageRevision(msg, ref))
	}
	return viewrecent.SummaryNode{
		ID:         viewrecent.NodeID(id),
		Scope:      testScope(),
		SourceRefs: refs,
		Summary:    text,
		Level:      level,
		Signature: views.ViewSignature{
			ViewID:             "summary-dag",
			SourceRevisions:    revisions,
			TransformSignature: "previous:v1",
		},
		CreatedAt: updatedAt,
		UpdatedAt: updatedAt,
	}
}

func assertValidSummaryNode(t *testing.T, node viewrecent.SummaryNode) {
	t.Helper()
	store := viewrecent.NewSummaryWorkspaceStore(sdkworkspace.NewMemWorkspace())
	if _, err := store.PutNode(context.Background(), node); err != nil {
		t.Fatalf("PutNode validation error = %v; node=%+v", err, node)
	}
}

func message(id string, seq uint64, role model.Role, text string, createdAt time.Time) sourcemessage.Message {
	return sourcemessage.Message{
		ID:             id,
		ConversationID: testScope().ConversationID,
		Seq:            seq,
		Message:        model.NewTextMessage(role, text),
		CreatedAt:      createdAt,
	}
}

func messageRef(msg sourcemessage.Message) views.SourceRef {
	return views.SourceRef{
		Kind: views.SourceMessage,
		Message: &views.MessageSourceRef{
			ConversationID: msg.ConversationID,
			MessageID:      msg.ID,
		},
	}
}

func messageRevision(msg sourcemessage.Message, ref views.SourceRef) views.SourceRevision {
	return views.SourceRevision{
		Kind:        views.SourceMessage,
		SourceKey:   ref.StableKey(),
		Revision:    strconv.FormatUint(msg.Seq, 10),
		ContentHash: MessageContentHash(msg),
		ObservedAt:  msg.CreatedAt,
	}
}

func testScope() views.Scope {
	return views.Scope{RuntimeID: "runtime-1", UserID: "user-1", ConversationID: "conversation-1"}
}

func testTime(offset int) time.Time {
	return time.Date(2026, 6, 15, 10, offset, 0, 0, time.UTC)
}
