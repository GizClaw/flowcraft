package mock

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

func TestGenerate_DefaultModel(t *testing.T) {
	m := &MockLLM{model: "mock-default", delay: 0}
	msg, usage, err := m.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "What is 2+2?"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Role != llm.RoleAssistant {
		t.Errorf("role = %q", msg.Role)
	}
	if !strings.Contains(msg.Content(), "What is 2+2?") {
		t.Errorf("content = %q, expected it to contain the user query", msg.Content())
	}
	if usage.TotalTokens != 30 {
		t.Errorf("total tokens = %d, want 30", usage.TotalTokens)
	}
}

func TestGenerate_ContextCancellation(t *testing.T) {
	m := &MockLLM{model: "mock-default", delay: 5 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := m.Generate(ctx, []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "hello"),
	})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestGenerate_EmptyMessages(t *testing.T) {
	m := &MockLLM{model: "mock-default", delay: 0}
	msg, _, err := m.Generate(context.Background(), []llm.Message{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg.Content(), "Mock response to:") {
		t.Errorf("content = %q", msg.Content())
	}
}

func TestGenerate_OnlySystemMessages(t *testing.T) {
	m := &MockLLM{model: "mock-default", delay: 0}
	msg, _, err := m.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, "You are a bot."),
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content() != "Mock response to: " {
		t.Errorf("content = %q", msg.Content())
	}
}

func TestGenerate_LongInputTruncated(t *testing.T) {
	m := &MockLLM{model: "mock-default", delay: 0}
	longInput := strings.Repeat("a", 200)
	msg, _, err := m.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, longInput),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(msg.Content(), "...") {
		t.Errorf("expected truncated content, got %q", msg.Content())
	}
}

func TestGenerateStream_ChunksAndCompletion(t *testing.T) {
	m := &MockLLM{model: "mock-default", delay: 0}
	stream, err := m.GenerateStream(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "hello world test"),
	})
	if err != nil {
		t.Fatal(err)
	}

	var chunks []string
	var lastFinishReason string
	for stream.Next() {
		c := stream.Current()
		chunks = append(chunks, c.Content)
		lastFinishReason = c.FinishReason
	}
	if stream.Err() != nil {
		t.Fatalf("stream error: %v", stream.Err())
	}

	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}
	if lastFinishReason != "stop" {
		t.Errorf("last finish_reason = %q, want stop", lastFinishReason)
	}

	msg := stream.Message()
	if msg.Role != llm.RoleAssistant {
		t.Errorf("role = %q", msg.Role)
	}
}

func TestGenerateStream_ContextCancellation(t *testing.T) {
	m := &MockLLM{model: "mock-default", delay: 5 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := m.GenerateStream(ctx, []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "hello"),
	})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestGenerateStream_Usage(t *testing.T) {
	m := &MockLLM{model: "mock-default", delay: 0}
	stream, err := m.GenerateStream(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "hi"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for stream.Next() {
	}

	usage := stream.Usage()
	if usage.InputTokens != 10 {
		t.Errorf("input tokens = %d, want 10", usage.InputTokens)
	}
	if usage.OutputTokens != 20 {
		t.Errorf("output tokens = %d, want 20", usage.OutputTokens)
	}
}

func TestGenerateStream_MessageAccumulation(t *testing.T) {
	m := &MockLLM{model: "mock-default", delay: 0}
	stream, err := m.GenerateStream(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "multi word query here please"),
	})
	if err != nil {
		t.Fatal(err)
	}

	for stream.Next() {
	}

	msg := stream.Message()
	if msg.Content() == "" {
		t.Error("accumulated message should not be empty")
	}
}

func TestE2E_ToolCallDispatch(t *testing.T) {
	m := &MockLLM{model: "mock-e2e", delay: 0}
	msg, _, err := m.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "[E2E_DISPATCH target=agent-42] Do something useful"),
	}, llm.WithTools(llm.ToolDefinition{Name: "kanban_submit"}))

	if err != nil {
		t.Fatal(err)
	}
	if !msg.HasToolCalls() {
		t.Fatal("expected tool call")
	}
	calls := msg.ToolCalls()
	if calls[0].Name != "kanban_submit" {
		t.Errorf("tool name = %q", calls[0].Name)
	}
	if !strings.Contains(calls[0].Arguments, "agent-42") {
		t.Errorf("args should contain target agent ID, got %q", calls[0].Arguments)
	}
}

func TestE2E_NoToolAvailable(t *testing.T) {
	m := &MockLLM{model: "mock-e2e", delay: 0}
	msg, _, err := m.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "[E2E_DISPATCH target=agent-1] do work"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.HasToolCalls() {
		t.Error("should not produce tool call without kanban_submit tool available")
	}
}

func TestE2E_ToolResultFollowup(t *testing.T) {
	m := &MockLLM{model: "mock-e2e", delay: 0}
	msg, _, err := m.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "[E2E_DISPATCH target=w1] build demo"),
		llm.NewToolResultMessage([]llm.ToolResult{
			{ToolCallID: "call_e2e_dispatch", Content: `{"target_agent_id":"w1","card_id":"card-1"}`},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content() != "E2E dispatch submitted. Waiting for callback." {
		t.Errorf("content = %q", msg.Content())
	}
}

func TestE2E_NonE2EModel(t *testing.T) {
	m := &MockLLM{model: "mock-fast", delay: 0}
	msg, _, err := m.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "[E2E_DISPATCH target=w1] test"),
	}, llm.WithTools(llm.ToolDefinition{Name: "kanban_submit"}))
	if err != nil {
		t.Fatal(err)
	}
	if msg.HasToolCalls() {
		t.Error("non-e2e model should not dispatch tool calls")
	}
}

func TestParseE2EDispatch(t *testing.T) {
	tests := []struct {
		input     string
		wantAgent string
		wantQuery string
		wantOK    bool
	}{
		{"[E2E_DISPATCH target=agent-1] do work", "agent-1", "do work", true},
		{"[E2E_DISPATCH target=abc] build a thing", "abc", "build a thing", true},
		{"not a dispatch", "", "", false},
		{"[E2E_DISPATCH] no target", "", "", false},
		{"[E2E_DISPATCH target=a]", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			agent, query, ok := parseE2EDispatch(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if agent != tt.wantAgent {
				t.Errorf("agent = %q, want %q", agent, tt.wantAgent)
			}
			if query != tt.wantQuery {
				t.Errorf("query = %q, want %q", query, tt.wantQuery)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input string
		max   int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hello..."},
		{"", 5, ""},
		{"abc", 3, "abc"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := truncate(tt.input, tt.max)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
			}
		})
	}
}

func TestHasTool(t *testing.T) {
	opts := &llm.GenerateOptions{
		Tools: []llm.ToolDefinition{
			{Name: "tool_a"},
			{Name: "tool_b"},
		},
	}
	if !hasTool(opts, "tool_a") {
		t.Error("expected true for tool_a")
	}
	if hasTool(opts, "tool_c") {
		t.Error("expected false for tool_c")
	}
	if hasTool(nil, "tool_a") {
		t.Error("expected false for nil opts")
	}
}

func TestLatestUserContent(t *testing.T) {
	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, "sys"),
		llm.NewTextMessage(llm.RoleUser, "first"),
		llm.NewTextMessage(llm.RoleAssistant, "reply"),
		llm.NewTextMessage(llm.RoleUser, "second"),
	}
	got := latestUserContent(msgs)
	if got != "second" {
		t.Errorf("got %q, want second", got)
	}
}

func TestLatestUserContent_NoUser(t *testing.T) {
	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, "sys"),
	}
	got := latestUserContent(msgs)
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestLatestMessage(t *testing.T) {
	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, "sys"),
		llm.NewTextMessage(llm.RoleUser, "user"),
		llm.NewTextMessage(llm.RoleSystem, "trailing sys"),
	}
	got := latestMessage(msgs)
	if got.Content() != "user" {
		t.Errorf("got %q, want user", got.Content())
	}
}

func TestLatestMessage_AllSystem(t *testing.T) {
	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, "only system"),
	}
	got := latestMessage(msgs)
	if got.Content() != "" {
		t.Errorf("got %q, want empty", got.Content())
	}
}

func TestMultiTurnConversation(t *testing.T) {
	m := &MockLLM{model: "mock-default", delay: 0}
	conversation := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, "You are a calculator."),
		llm.NewTextMessage(llm.RoleUser, "What is 2+2?"),
	}

	msg1, _, err := m.Generate(context.Background(), conversation)
	if err != nil {
		t.Fatal(err)
	}

	conversation = append(conversation, msg1)
	conversation = append(conversation, llm.NewTextMessage(llm.RoleUser, "And what is 3+3?"))

	msg2, _, err := m.Generate(context.Background(), conversation)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(msg2.Content(), "3+3") {
		t.Errorf("second reply should reference latest user msg, got %q", msg2.Content())
	}
}

func TestMockStream_Close(t *testing.T) {
	m := &MockLLM{model: "mock-default", delay: 0}
	stream, err := m.GenerateStream(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "hi"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := stream.Close(); err != nil {
		t.Errorf("close error: %v", err)
	}
}

func TestMockStream_ErrAlwaysNil(t *testing.T) {
	m := &MockLLM{model: "mock-default", delay: 0}
	stream, err := m.GenerateStream(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "hi"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for stream.Next() {
	}
	if stream.Err() != nil {
		t.Errorf("err = %v", stream.Err())
	}
}
