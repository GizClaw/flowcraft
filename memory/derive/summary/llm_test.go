package summary

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/memory/derive"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	viewrecent "github.com/GizClaw/flowcraft/memory/views/recent"
	sdkllm "github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestLLMSummarizerGeneratesDeterministicLeafChunks(t *testing.T) {
	ctx := context.Background()
	fake := &summaryFakeLLM{reply: `{"summary":"LLM leaf summary."}`}
	msgs := numberedSummaryMessages(7)
	input := summaryInput(msgs, nil, derive.SummaryPolicy{MaxRawMessages: 1, PreserveRecentMessages: 1, MaxSummaryBytes: 4096})

	nodes, err := (LLMSummarizer{LLM: fake, MaxSourceRefsPerNode: 3}).Summarize(ctx, input)
	if err != nil {
		t.Fatalf("Summarize error = %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("nodes len = %d, want two depth-0 leaf chunks", len(nodes))
	}
	for i, node := range nodes {
		if node.Level != 0 || len(node.ParentIDs) != 0 {
			t.Fatalf("node[%d] level/parents = %d/%+v, want depth-0 leaf", i, node.Level, node.ParentIDs)
		}
		if !strings.HasPrefix(string(node.ID), "summary-llm-") || strings.HasPrefix(string(node.ID), "summary-buffer-") {
			t.Fatalf("node[%d] ID = %q, want summary-llm prefix", i, node.ID)
		}
		if node.Metadata["algorithm"] != llmAlgorithm || node.Metadata["node_kind"] != "leaf" || node.Metadata["depth"] != 0 {
			t.Fatalf("node[%d] metadata = %+v, want leaf llm metadata", i, node.Metadata)
		}
		assertValidSummaryNode(t, node)
	}
	if got := sourceRefMessageIDs(nodes[0].SourceRefs); strings.Join(got, ",") != "msg-1,msg-2,msg-3" {
		t.Fatalf("first leaf SourceRefs = %v, want msg-1..msg-3", got)
	}
	if got := sourceRefMessageIDs(nodes[1].SourceRefs); strings.Join(got, ",") != "msg-4,msg-5,msg-6" {
		t.Fatalf("second leaf SourceRefs = %v, want msg-4..msg-6", got)
	}
	if strings.Contains(nodes[0].Summary, "Message 7") || strings.Contains(nodes[1].Summary, "Message 7") {
		t.Fatalf("summary includes preserved raw message: %+v", nodes)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("LLM calls = %d, want one call per leaf chunk", len(fake.calls))
	}
	if fake.calls[0].jsonMode == nil || !*fake.calls[0].jsonMode {
		t.Fatalf("JSONMode = %v, want true", fake.calls[0].jsonMode)
	}
	if got := len(fake.calls[0].messages); got != 5 {
		t.Fatalf("first LLM messages len = %d, want system + control + three source messages", got)
	}
	control := fake.calls[0].messages[1].Content()
	if !strings.Contains(control, `"message_count": 3`) || !strings.Contains(control, `"msg-1"`) || strings.Contains(control, "Message 1") {
		t.Fatalf("first LLM control = %s, want source ids without flattened message text", control)
	}
	if got := promptSourceIDsFromCall(t, fake.calls[0], 2); strings.Join(got, ",") != "msg-1,msg-2,msg-3" {
		t.Fatalf("first LLM source messages = %v, want first deterministic leaf chunk only", got)
	}
	if got := fake.calls[0].messages[2].Content(); got != "Message 1" {
		t.Fatalf("first source message content = %q, want original source text", got)
	}
}

func TestLLMSummarizerCondensesSameDepthNodesAtFanout(t *testing.T) {
	ctx := context.Background()
	fake := &summaryFakeLLM{reply: `{"summary":"LLM summary."}`}
	msgs := numberedSummaryMessages(13)
	input := summaryInput(msgs, nil, derive.SummaryPolicy{MaxRawMessages: 1, PreserveRecentMessages: 1, MaxSummaryBytes: 8192})

	nodes, err := (LLMSummarizer{LLM: fake, MaxSourceRefsPerNode: 3, CondenseFanout: 4}).Summarize(ctx, input)
	if err != nil {
		t.Fatalf("Summarize error = %v", err)
	}
	if len(nodes) != 5 {
		t.Fatalf("nodes len = %d, want four leaves plus one condensed node", len(nodes))
	}
	condensed := nodes[4]
	if condensed.Level != 1 {
		t.Fatalf("condensed level = %d, want 1", condensed.Level)
	}
	if got, want := condensed.ParentIDs, []viewrecent.NodeID{nodes[0].ID, nodes[1].ID, nodes[2].ID, nodes[3].ID}; !nodeIDsEqual(got, want) {
		t.Fatalf("condensed ParentIDs = %v, want child leaf IDs %v", got, want)
	}
	if got := sourceRefMessageIDs(condensed.SourceRefs); strings.Join(got, ",") != "msg-1,msg-2,msg-3,msg-4,msg-5,msg-6,msg-7,msg-8,msg-9,msg-10,msg-11,msg-12" {
		t.Fatalf("condensed SourceRefs = %v, want stable raw source order", got)
	}
	if condensed.Metadata["node_kind"] != "condensed" ||
		condensed.Metadata["depth"] != 1 ||
		condensed.Metadata["parent_ids_semantics"] != "compaction_input_summary_node_ids" ||
		condensed.Metadata["input_summary_node_count"] != 4 ||
		condensed.Metadata["source_ref_count"] != 12 {
		t.Fatalf("condensed metadata = %+v, want layered compaction metadata", condensed.Metadata)
	}
	if got := len(fake.calls[4].messages); got != 2 {
		t.Fatalf("condensed LLM messages len = %d, want system + control payload only", got)
	}
	if payload := fake.calls[4].messages[1].Content(); !strings.Contains(payload, `"task": "condensed_summary"`) || !strings.Contains(payload, string(nodes[0].ID)) {
		t.Fatalf("condensed LLM payload = %s, want child summary chunk", payload)
	}
	assertValidSummaryNode(t, condensed)
}

func TestLLMSummarizerIsIdempotentWithExistingLeafAndCondensedNodes(t *testing.T) {
	ctx := context.Background()
	fake := &summaryFakeLLM{reply: `{"summary":"LLM summary."}`}
	msgs := numberedSummaryMessages(13)
	input := summaryInput(msgs, nil, derive.SummaryPolicy{MaxRawMessages: 1, PreserveRecentMessages: 1})

	nodes, err := (LLMSummarizer{LLM: fake, MaxSourceRefsPerNode: 3, CondenseFanout: 4}).Summarize(ctx, input)
	if err != nil {
		t.Fatalf("initial Summarize error = %v", err)
	}
	againInput := summaryInput(msgs, nodes, derive.SummaryPolicy{MaxRawMessages: 1, PreserveRecentMessages: 1})
	again, err := (LLMSummarizer{LLM: fake, MaxSourceRefsPerNode: 3, CondenseFanout: 4}).Summarize(ctx, againInput)
	if err != nil {
		t.Fatalf("second Summarize error = %v", err)
	}
	if again != nil {
		t.Fatalf("second Summarize = %+v, want nil without duplicate leaf or condensed chunks", again)
	}
	if len(fake.calls) != 5 {
		t.Fatalf("LLM calls after idempotent run = %d, want unchanged 5", len(fake.calls))
	}
}

func TestLLMSummarizerFallbackDoesNotAffectSourceRefCoverage(t *testing.T) {
	ctx := context.Background()
	fake := &summaryFakeLLM{reply: `{"nodes":[{"source_ids":["msg-1"],"summary":""}]}`}
	msgs := numberedSummaryMessages(4)
	input := summaryInput(msgs, nil, derive.SummaryPolicy{MaxRawMessages: 1, PreserveRecentMessages: 1})

	nodes, err := (LLMSummarizer{LLM: fake, MaxSourceRefsPerNode: 3}).Summarize(ctx, input)
	if err != nil {
		t.Fatalf("Summarize error = %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("nodes len = %d, want one deterministic leaf", len(nodes))
	}
	if got := sourceRefMessageIDs(nodes[0].SourceRefs); strings.Join(got, ",") != "msg-1,msg-2,msg-3" {
		t.Fatalf("SourceRefs = %v, want full deterministic chunk despite LLM omission", got)
	}
	if nodes[0].Metadata["llm_fallback_deterministic"] != true {
		t.Fatalf("metadata = %+v, want deterministic fallback marker", nodes[0].Metadata)
	}
	if !strings.Contains(nodes[0].Summary, "Message 2") || strings.Contains(nodes[0].Summary, "Message 4") {
		t.Fatalf("fallback Summary = %q, want folded chunk only", nodes[0].Summary)
	}
	if strings.HasPrefix(string(nodes[0].ID), "summary-buffer-") {
		t.Fatalf("LLM node ID = %q, must not use summary-buffer prefix", nodes[0].ID)
	}
	assertValidSummaryNode(t, nodes[0])
}

func TestLLMSummarizerSkipsMessagesCoveredByAnyCurrentNode(t *testing.T) {
	ctx := context.Background()
	fake := &summaryFakeLLM{reply: `{"summary":"Caroline researched adoption agencies."}`}
	msg1 := message("msg-1", 1, model.RoleUser, "Already covered one.", testTime(1))
	msg2 := message("msg-2", 2, model.RoleAssistant, "Already covered two.", testTime(2))
	msg3 := message("msg-3", 3, model.RoleUser, "Caroline researched adoption agencies.", testTime(3))
	msg4 := message("msg-4", 4, model.RoleAssistant, "New folded fact.", testTime(4))
	msg5 := message("msg-5", 5, model.RoleUser, "Another folded fact.", testTime(5))
	msg6 := message("msg-6", 6, model.RoleAssistant, "Keep raw.", testTime(6))
	current := []viewrecent.SummaryNode{
		summaryNode("covered-1", []sourcemessage.Message{msg1}, "covered one", 0, testTime(10)),
		summaryNode("covered-2", []sourcemessage.Message{msg2}, "covered two", 0, testTime(11)),
	}
	input := summaryInput([]sourcemessage.Message{msg1, msg2, msg3, msg4, msg5, msg6}, current, derive.SummaryPolicy{MaxRawMessages: 1, PreserveRecentMessages: 1})

	nodes, err := (LLMSummarizer{LLM: fake, MaxSourceRefsPerNode: 3}).Summarize(ctx, input)
	if err != nil {
		t.Fatalf("Summarize error = %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("nodes len = %d, want one uncovered leaf chunk", len(nodes))
	}
	if got := sourceRefMessageIDs(nodes[0].SourceRefs); strings.Join(got, ",") != "msg-3,msg-4,msg-5" {
		t.Fatalf("SourceRefs = %v, want only uncovered folded messages", got)
	}
	control := fake.calls[0].messages[1].Content()
	if strings.Contains(control, "msg-1") || strings.Contains(control, "msg-2") || !strings.Contains(control, "msg-3") {
		t.Fatalf("LLM control = %s, want only uncovered folded message chunk", control)
	}
	if got := promptSourceIDsFromCall(t, fake.calls[0], 2); strings.Join(got, ",") != "msg-3,msg-4,msg-5" {
		t.Fatalf("LLM source messages = %v, want only uncovered folded message chunk", got)
	}
}

type summaryFakeLLM struct {
	reply string
	calls []summaryFakeLLMCall
}

type summaryFakeLLMCall struct {
	messages []sdkllm.Message
	jsonMode *bool
}

func (f *summaryFakeLLM) Generate(_ context.Context, messages []sdkllm.Message, opts ...sdkllm.GenerateOption) (sdkllm.Message, sdkllm.TokenUsage, error) {
	applied := sdkllm.ApplyOptions(opts...)
	f.calls = append(f.calls, summaryFakeLLMCall{
		messages: append([]sdkllm.Message(nil), messages...),
		jsonMode: applied.JSONMode,
	})
	return sdkllm.NewTextMessage(sdkllm.RoleAssistant, f.reply), sdkllm.TokenUsage{}, nil
}

func (f *summaryFakeLLM) GenerateStream(context.Context, []sdkllm.Message, ...sdkllm.GenerateOption) (sdkllm.StreamMessage, error) {
	return nil, nil
}

func promptSourceIDsFromCall(t *testing.T, call summaryFakeLLMCall, start int) []string {
	t.Helper()
	out := make([]string, 0, len(call.messages)-start)
	for _, msg := range call.messages[start:] {
		metadata := promptSourceMetadata(t, msg)
		sourceID, _ := metadata["source_id"].(string)
		out = append(out, sourceID)
	}
	return out
}

func promptSourceMetadata(t *testing.T, msg sdkllm.Message) map[string]any {
	t.Helper()
	if len(msg.Parts) == 0 {
		t.Fatalf("message has no parts: %+v", msg)
	}
	part := msg.Parts[len(msg.Parts)-1]
	if part.Type != model.PartData || part.Data == nil {
		t.Fatalf("last part = %+v, want source metadata data part", part)
	}
	if part.Data.MimeType != sourcemessage.PromptSourceMessageMIMEType {
		t.Fatalf("metadata MIME = %q, want %q", part.Data.MimeType, sourcemessage.PromptSourceMessageMIMEType)
	}
	return part.Data.Value
}

func numberedSummaryMessages(n int) []sourcemessage.Message {
	out := make([]sourcemessage.Message, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, message("msg-"+strconv.Itoa(i), uint64(i), model.RoleUser, "Message "+strconv.Itoa(i), testTime(i)))
	}
	return out
}
