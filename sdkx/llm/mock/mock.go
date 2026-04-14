// Package mock provides a deterministic LLM provider for E2E and integration testing.
// Register it by importing with a blank identifier: _ "github.com/GizClaw/flowcraft/sdkx/llm/mock"
// Or enable it at runtime via FLOWCRAFT_LLM_MOCK=true.
package mock

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

func init() {
	llm.RegisterProvider("mock", func(model string, config map[string]any) (llm.LLM, error) {
		return &MockLLM{model: model, delay: 50 * time.Millisecond}, nil
	})

	llm.RegisterProviderModels("mock", []llm.ModelInfo{
		{Label: "Mock Default", Name: "mock-default"},
		{Label: "Mock Fast", Name: "mock-fast"},
		{Label: "Mock E2E", Name: "mock-e2e"},
	})
}

// MockLLM returns deterministic responses for testing.
type MockLLM struct {
	model string
	delay time.Duration
}

func (m *MockLLM) Generate(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	select {
	case <-ctx.Done():
		return llm.Message{}, llm.TokenUsage{}, ctx.Err()
	case <-time.After(m.delay):
	}

	if msg, ok := m.generateE2EResponse(messages, llm.ApplyOptions(opts...)); ok {
		usage := llm.TokenUsage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30}
		return msg, usage, nil
	}

	lastContent := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == llm.RoleUser {
			lastContent = messages[i].Content()
			break
		}
	}

	reply := fmt.Sprintf("Mock response to: %s", truncate(lastContent, 100))
	msg := llm.Message{
		Role:  llm.RoleAssistant,
		Parts: []llm.Part{{Type: llm.PartText, Text: reply}},
	}
	usage := llm.TokenUsage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30}
	return msg, usage, nil
}

func (m *MockLLM) GenerateStream(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	msg, usage, err := m.Generate(ctx, messages, opts...)
	if err != nil {
		return nil, err
	}
	return &mockStream{
		msg:   msg,
		text:  msg.Content(),
		usage: llm.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens},
	}, nil
}

type mockStream struct {
	msg     llm.Message
	text    string
	usage   llm.Usage
	chunks  []string
	idx     int
	once    sync.Once
	current llm.StreamChunk
}

func (s *mockStream) init() {
	words := strings.Fields(s.text)
	for i := 0; i < len(words); i += 3 {
		end := i + 3
		if end > len(words) {
			end = len(words)
		}
		s.chunks = append(s.chunks, strings.Join(words[i:end], " "))
	}
	if len(s.chunks) == 0 {
		s.chunks = []string{s.text}
	}
}

func (s *mockStream) Next() bool {
	s.once.Do(s.init)
	if s.idx >= len(s.chunks) {
		return false
	}
	content := s.chunks[s.idx]
	if s.idx > 0 {
		content = " " + content
	}
	s.current = llm.StreamChunk{
		Role:    llm.RoleAssistant,
		Content: content,
	}
	s.idx++
	if s.idx >= len(s.chunks) {
		s.current.FinishReason = "stop"
	}
	return true
}

func (s *mockStream) Current() llm.StreamChunk { return s.current }
func (s *mockStream) Err() error               { return nil }
func (s *mockStream) Close() error             { return nil }

func (s *mockStream) Message() llm.Message {
	return s.msg
}

func (s *mockStream) Usage() llm.Usage { return s.usage }

func (m *MockLLM) generateE2EResponse(messages []llm.Message, opts *llm.GenerateOptions) (llm.Message, bool) {
	if m.model != "mock-e2e" {
		return llm.Message{}, false
	}

	latest := latestMessage(messages)
	if latest.Role == llm.RoleTool && hasToolResult(latest, "kanban_submit") {
		return llm.NewTextMessage(llm.RoleAssistant, "E2E dispatch submitted. Waiting for callback."), true
	}

	lastUser := latestUserContent(messages)
	if strings.HasPrefix(lastUser, "[Task Callback]") {
		return llm.NewTextMessage(llm.RoleAssistant, "E2E callback processed successfully."), true
	}

	targetAgentID, query, ok := parseE2EDispatch(lastUser)
	if ok && hasTool(opts, "kanban_submit") {
		args, _ := llm.MarshalToolArgs(map[string]string{
			"target_agent_id": targetAgentID,
			"query":           query,
			"user_query":      query,
			"dispatch_note":   "Return a short summary for the user after completion.",
		})
		return llm.NewToolCallMessage([]llm.ToolCall{{
			ID:        "call_e2e_dispatch",
			Name:      "kanban_submit",
			Arguments: args,
		}}), true
	}

	return llm.Message{}, false
}

func latestUserContent(messages []llm.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == llm.RoleUser {
			return messages[i].Content()
		}
	}
	return ""
}

func latestMessage(messages []llm.Message) llm.Message {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != llm.RoleSystem {
			return messages[i]
		}
	}
	return llm.Message{}
}

func hasTool(opts *llm.GenerateOptions, name string) bool {
	if opts == nil {
		return false
	}
	for _, tool := range opts.Tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func hasToolResult(msg llm.Message, name string) bool {
	if msg.Role != llm.RoleTool {
		return false
	}
	for _, part := range msg.Parts {
		if part.Type == llm.PartToolResult && part.ToolResult != nil {
			if strings.Contains(part.ToolResult.Content, `"target_agent_id":"`) || strings.Contains(part.ToolResult.Content, `"card_id":"`) {
				if name == "kanban_submit" {
					return true
				}
			}
		}
	}
	return false
}

func parseE2EDispatch(content string) (targetAgentID, query string, ok bool) {
	const prefix = "[E2E_DISPATCH"
	if !strings.HasPrefix(content, prefix) {
		return "", "", false
	}
	end := strings.Index(content, "]")
	if end == -1 {
		return "", "", false
	}
	header := content[len(prefix):end]
	body := strings.TrimSpace(content[end+1:])
	for _, part := range strings.Fields(header) {
		if strings.HasPrefix(part, "target=") {
			targetAgentID = strings.TrimPrefix(part, "target=")
		}
	}
	if targetAgentID == "" || body == "" {
		return "", "", false
	}
	return targetAgentID, body, true
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
