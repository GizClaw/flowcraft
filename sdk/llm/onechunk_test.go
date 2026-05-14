package llm

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestOneChunkStream_TextMessage(t *testing.T) {
	msg := NewTextMessage(RoleAssistant, "hello world")
	usage := TokenUsage{InputTokens: 7, OutputTokens: 3}

	stream := NewOneChunkStream(msg, usage)

	if !stream.Next() {
		t.Fatal("first Next() should return true")
	}
	cur := stream.Current()
	if cur.Role != RoleAssistant {
		t.Errorf("Current().Role = %q, want assistant", cur.Role)
	}
	if cur.Content != "hello world" {
		t.Errorf("Current().Content = %q", cur.Content)
	}
	if cur.FinishReason != "stop" {
		t.Errorf("Current().FinishReason = %q, want stop", cur.FinishReason)
	}

	if stream.Next() {
		t.Fatal("second Next() should return false")
	}
	if err := stream.Err(); err != nil {
		t.Errorf("Err() = %v", err)
	}
	if got := stream.Message(); got.Content() != "hello world" {
		t.Errorf("Message().Content() = %q", got.Content())
	}
	if u := stream.Usage(); u.InputTokens != 7 || u.OutputTokens != 3 {
		t.Errorf("Usage = %+v", u)
	}
	if err := stream.Close(); err != nil {
		t.Errorf("Close() = %v", err)
	}
	// Close is idempotent.
	if err := stream.Close(); err != nil {
		t.Errorf("second Close() = %v", err)
	}
}

func TestOneChunkStream_ToolCallsReplayed(t *testing.T) {
	msg := NewToolCallMessage([]ToolCall{
		{ID: "c1", Name: "search", Arguments: `{"q":"x"}`},
		{ID: "c2", Name: "fetch", Arguments: `{}`},
	})
	stream := NewOneChunkStream(msg, TokenUsage{})

	if !stream.Next() {
		t.Fatal("Next() = false")
	}
	cur := stream.Current()
	if len(cur.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %d, want 2", len(cur.ToolCalls))
	}
	if cur.ToolCalls[0].Name != "search" || cur.ToolCalls[1].Name != "fetch" {
		t.Errorf("tool call order/name lost: %+v", cur.ToolCalls)
	}
}

func TestOneChunkStream_PropagatesCachedInputTokens(t *testing.T) {
	// Regression for #136: model.Usage now carries CachedInputTokens
	// so the synchronous-only Generate → NewOneChunkStream adapter
	// must surface it to callers that observe via Usage() (e.g.
	// llmnode → host.ReportUsage).
	msg := NewTextMessage(RoleAssistant, "hi")
	usage := TokenUsage{InputTokens: 100, CachedInputTokens: 80, OutputTokens: 5, TotalTokens: 105}
	stream := NewOneChunkStream(msg, usage)
	if !stream.Next() {
		t.Fatal("Next() = false")
	}
	got := stream.Usage()
	if got.InputTokens != 100 || got.CachedInputTokens != 80 || got.OutputTokens != 5 {
		t.Errorf("Usage = %+v, want {Input:100 Cached:80 Output:5}", got)
	}
}

func TestOneChunkStream_NonTextPartsAreInMessageNotChunk(t *testing.T) {
	// Multimodal output: chunk only carries text concat (empty here);
	// callers must read Message() for image parts.
	msg := Message{
		Role: RoleAssistant,
		Parts: []model.Part{
			{Type: model.PartImage, Image: &model.MediaRef{URL: "https://cdn/x.png"}},
			{Type: model.PartText, Text: "caption"},
		},
	}
	stream := NewOneChunkStream(msg, TokenUsage{})

	stream.Next()
	cur := stream.Current()
	if cur.Content != "caption" {
		t.Errorf("Current().Content = %q, want only the text part replayed", cur.Content)
	}

	finalMsg := stream.Message()
	if len(finalMsg.Parts) != 2 {
		t.Fatalf("Message().Parts = %d, want 2", len(finalMsg.Parts))
	}
	if finalMsg.Parts[0].Image == nil || finalMsg.Parts[0].Image.URL != "https://cdn/x.png" {
		t.Errorf("image part not preserved in final Message: %+v", finalMsg.Parts[0])
	}
}
