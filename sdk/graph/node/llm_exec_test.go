package node

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// --- mock LLM & stream ---

type mockLLM struct {
	msg   model.Message
	usage model.Usage
	err   error
}

func (m *mockLLM) Generate(_ context.Context, _ []model.Message, _ ...llm.GenerateOption) (model.Message, model.TokenUsage, error) {
	return m.msg, model.TokenUsage{}, m.err
}

func (m *mockLLM) GenerateStream(_ context.Context, _ []model.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &mockStream{msg: m.msg, usage: m.usage}, nil
}

type mockStream struct {
	msg     model.Message
	usage   model.Usage
	chunks  []model.StreamChunk
	idx     int
	errOnce error
}

func (s *mockStream) Next() bool {
	if s.idx < len(s.chunks) {
		s.idx++
		return true
	}
	return false
}

func (s *mockStream) Current() model.StreamChunk {
	return s.chunks[s.idx-1]
}

func (s *mockStream) Err() error   { return s.errOnce }
func (s *mockStream) Close() error { return nil }
func (s *mockStream) Message() model.Message {
	return s.msg
}
func (s *mockStream) Usage() model.Usage {
	return s.usage
}

// --- mock resolver ---

type mockResolver struct {
	llmInst llm.LLM
	err     error
}

func (r *mockResolver) Resolve(_ context.Context, _ string) (llm.LLM, error) {
	return r.llmInst, r.err
}

func (r *mockResolver) InvalidateCache(_ string) {}

// --- mock tool ---

type mockTool struct {
	name   string
	result string
	err    error
}

func (t *mockTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: t.name, Description: "mock"}
}

func (t *mockTool) Execute(_ context.Context, _ string) (string, error) {
	return t.result, t.err
}

// --- helpers ---

func execCtx() graph.ExecutionContext {
	return graph.ExecutionContext{Context: context.Background()}
}

func execCtxWithStream(cb graph.StreamCallback) graph.ExecutionContext {
	return graph.ExecutionContext{Context: context.Background(), Stream: cb}
}

// --- LLMNode.Config ---

func TestLLMNode_Config(t *testing.T) {
	n := NewLLMNode("n", nil, nil, LLMConfig{})
	n.rawConfig = map[string]any{"model": "gpt-4"}
	c := n.Config()
	if c["model"] != "gpt-4" {
		t.Fatalf("Config()[model] = %v", c["model"])
	}
}

// --- ExecuteBoard: basic happy path ---

func TestLLMNode_ExecuteBoard_Basic(t *testing.T) {
	resolver := &mockResolver{llmInst: &mockLLM{
		msg:   model.NewTextMessage(model.RoleAssistant, "hello world"),
		usage: model.Usage{InputTokens: 10, OutputTokens: 5},
	}}

	n := NewLLMNode("llm1", resolver, nil, LLMConfig{
		SystemPrompt: "be helpful",
	})

	board := graph.NewBoard()
	board.SetChannel(workflow.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "hi"),
	})

	err := n.ExecuteBoard(execCtx(), board)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	resp, ok := board.GetVar(VarResponse)
	if !ok {
		t.Fatal("response var not set")
	}
	if resp != "" {
		t.Log("response set (stream had no chunks, so empty is expected)")
	}

	tp, _ := board.GetVar(VarToolPending)
	if tp != false {
		t.Fatalf("tool_pending = %v, want false", tp)
	}
}

// --- ExecuteBoard: streaming with content chunks ---

func TestLLMNode_ExecuteBoard_WithChunks(t *testing.T) {
	stream := &mockStream{
		chunks: []model.StreamChunk{
			{Content: "hello "},
			{Content: "world"},
		},
		msg:   model.Message{},
		usage: model.Usage{InputTokens: 5, OutputTokens: 3},
	}

	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}

	n := NewLLMNode("llm1", resolver, nil, LLMConfig{})
	board := graph.NewBoard()
	board.SetChannel(workflow.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "test"),
	})

	var tokens []string
	ctx := execCtxWithStream(func(ev graph.StreamEvent) {
		if ev.Type == "token" {
			if p, ok := ev.Payload.(map[string]any); ok {
				tokens = append(tokens, p["content"].(string))
			}
		}
	})

	err := n.ExecuteBoard(ctx, board)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tokens) != 2 || tokens[0] != "hello " || tokens[1] != "world" {
		t.Fatalf("tokens = %v", tokens)
	}

	resp, _ := board.GetVar(VarResponse)
	if resp != "hello world" {
		t.Fatalf("response = %q", resp)
	}
}

type streamOnlyLLM struct {
	stream llm.StreamMessage
}

func (s *streamOnlyLLM) Generate(_ context.Context, _ []model.Message, _ ...llm.GenerateOption) (model.Message, model.TokenUsage, error) {
	return model.Message{}, model.TokenUsage{}, nil
}

func (s *streamOnlyLLM) GenerateStream(_ context.Context, _ []model.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return s.stream, nil
}

// --- ExecuteBoard: resolve error ---

func TestLLMNode_ExecuteBoard_ResolveError(t *testing.T) {
	resolver := &mockResolver{err: errors.New("no model")}
	n := NewLLMNode("llm1", resolver, nil, LLMConfig{Model: "bad"})

	board := graph.NewBoard()
	board.SetChannel(workflow.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "hi"),
	})

	err := n.ExecuteBoard(execCtx(), board)
	if err == nil {
		t.Fatal("expected error")
	}
}

// --- ExecuteBoard: generate stream error ---

func TestLLMNode_ExecuteBoard_GenerateStreamError(t *testing.T) {
	resolver := &mockResolver{llmInst: &mockLLM{err: errors.New("stream fail")}}
	n := NewLLMNode("llm1", resolver, nil, LLMConfig{})

	board := graph.NewBoard()
	board.SetChannel(workflow.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "hi"),
	})

	err := n.ExecuteBoard(execCtx(), board)
	if err == nil {
		t.Fatal("expected error")
	}
}

// --- ExecuteBoard: stream iteration error ---

func TestLLMNode_ExecuteBoard_StreamIterError(t *testing.T) {
	stream := &mockStream{errOnce: errors.New("mid-stream fail")}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	n := NewLLMNode("llm1", resolver, nil, LLMConfig{})

	board := graph.NewBoard()
	board.SetChannel(workflow.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "hi"),
	})

	err := n.ExecuteBoard(execCtx(), board)
	if err == nil {
		t.Fatal("expected stream error")
	}
}

// --- ExecuteBoard: JSON mode ---

func TestLLMNode_ExecuteBoard_JSONMode(t *testing.T) {
	stream := &mockStream{
		chunks: []model.StreamChunk{
			{Content: `{"intent": "qa"}`},
		},
		msg:   model.Message{},
		usage: model.Usage{},
	}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	n := NewLLMNode("llm1", resolver, nil, LLMConfig{
		JSONMode:  true,
		OutputKey: "result",
	})

	board := graph.NewBoard()
	board.SetChannel(workflow.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "classify"),
	})

	err := n.ExecuteBoard(execCtx(), board)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	val, ok := board.GetVar("result")
	if !ok {
		t.Fatal("result var not set")
	}
	m, ok := val.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", val)
	}
	if m["intent"] != "qa" {
		t.Fatalf("intent = %v", m["intent"])
	}
}

// --- ExecuteBoard: JSON mode with invalid JSON ---

func TestLLMNode_ExecuteBoard_JSONMode_InvalidJSON(t *testing.T) {
	stream := &mockStream{
		chunks: []model.StreamChunk{
			{Content: "not json at all"},
		},
		msg:   model.Message{},
		usage: model.Usage{},
	}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	n := NewLLMNode("llm1", resolver, nil, LLMConfig{
		JSONMode:  true,
		OutputKey: "result",
	})

	board := graph.NewBoard()
	board.SetVar("result", "pre-existing")
	board.SetChannel(workflow.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "bad"),
	})

	err := n.ExecuteBoard(execCtx(), board)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	val, _ := board.GetVar("result")
	if val != "pre-existing" {
		t.Fatalf("expected pre-existing value preserved, got %v", val)
	}
}

// --- ExecuteBoard: JSON mode with scalar value ---

func TestLLMNode_ExecuteBoard_JSONMode_Scalar(t *testing.T) {
	stream := &mockStream{
		chunks: []model.StreamChunk{
			{Content: `42`},
		},
		msg:   model.Message{},
		usage: model.Usage{},
	}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	n := NewLLMNode("llm1", resolver, nil, LLMConfig{
		JSONMode:  true,
		OutputKey: "result",
	})

	board := graph.NewBoard()
	board.SetVar("result", "pre-existing")
	board.SetChannel(workflow.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "hi"),
	})

	err := n.ExecuteBoard(execCtx(), board)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	val, _ := board.GetVar("result")
	if val != "pre-existing" {
		t.Fatalf("scalar JSON should keep existing value, got %v", val)
	}
}

// --- ExecuteBoard: custom messages_key ---

func TestLLMNode_ExecuteBoard_CustomMessagesKey(t *testing.T) {
	stream := &mockStream{
		chunks: []model.StreamChunk{{Content: "ok"}},
		msg:    model.Message{},
		usage:  model.Usage{},
	}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	n := NewLLMNode("llm1", resolver, nil, LLMConfig{
		MessagesKey: "custom_msgs",
	})

	board := graph.NewBoard()
	board.SetVar("custom_msgs", []model.Message{
		model.NewTextMessage(model.RoleUser, "via var"),
	})

	err := n.ExecuteBoard(execCtx(), board)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- ExecuteBoard: query fallback ---

func TestLLMNode_ExecuteBoard_QueryFallback(t *testing.T) {
	stream := &mockStream{
		chunks: []model.StreamChunk{{Content: "resp"}},
		msg:    model.Message{},
		usage:  model.Usage{},
	}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	n := NewLLMNode("llm1", resolver, nil, LLMConfig{
		MessagesKey:   "alt_msgs",
		QueryFallback: true,
	})

	board := graph.NewBoard()
	board.SetVar(workflow.VarQuery, "what is this?")

	err := n.ExecuteBoard(execCtx(), board)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- ExecuteBoard: query fallback with duplicate query ---

func TestLLMNode_ExecuteBoard_QueryFallback_NoDuplicate(t *testing.T) {
	stream := &mockStream{
		chunks: []model.StreamChunk{{Content: "resp"}},
		msg:    model.Message{},
		usage:  model.Usage{},
	}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	n := NewLLMNode("llm1", resolver, nil, LLMConfig{
		MessagesKey:   "alt_msgs",
		QueryFallback: true,
	})

	board := graph.NewBoard()
	board.SetVar(workflow.VarQuery, "hello")
	board.SetVar("alt_msgs", []model.Message{
		model.NewTextMessage(model.RoleUser, "hello"),
	})

	err := n.ExecuteBoard(execCtx(), board)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- ExecuteBoard: track steps ---

func TestLLMNode_ExecuteBoard_TrackSteps(t *testing.T) {
	stream := &mockStream{
		chunks: []model.StreamChunk{{Content: "step1"}},
		msg:    model.Message{},
		usage:  model.Usage{InputTokens: 1, OutputTokens: 2},
	}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	n := NewLLMNode("llm1", resolver, nil, LLMConfig{TrackSteps: true})

	board := graph.NewBoard()
	board.SetChannel(workflow.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "hi"),
	})

	err := n.ExecuteBoard(execCtx(), board)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	steps, ok := board.GetVar("agent_steps")
	if !ok {
		t.Fatal("agent_steps not set")
	}
	sl := steps.([]map[string]any)
	if len(sl) != 1 {
		t.Fatalf("expected 1 step, got %d", len(sl))
	}
}

// --- ExecuteBoard: accumulates usage ---

func TestLLMNode_ExecuteBoard_AccumulateUsage(t *testing.T) {
	stream := &mockStream{
		chunks: []model.StreamChunk{{Content: "x"}},
		msg:    model.Message{},
		usage:  model.Usage{InputTokens: 10, OutputTokens: 5},
	}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	n := NewLLMNode("llm1", resolver, nil, LLMConfig{})

	board := graph.NewBoard()
	board.SetVar(workflow.VarInternalUsage, model.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150})
	board.SetChannel(workflow.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "hi"),
	})

	err := n.ExecuteBoard(execCtx(), board)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	u, _ := board.GetVar(workflow.VarInternalUsage)
	usage := u.(model.TokenUsage)
	if usage.InputTokens != 110 {
		t.Fatalf("accumulated InputTokens = %d, want 110", usage.InputTokens)
	}
}

// --- ExecuteBoard: tool calls ---

func TestLLMNode_ExecuteBoard_WithToolCalls(t *testing.T) {
	tc := model.ToolCall{ID: "tc1", Name: "search", Arguments: `{"q":"hello"}`}
	accMsg := model.Message{
		Role: model.RoleAssistant,
		Parts: []model.Part{
			{Type: model.PartToolCall, ToolCall: &tc},
		},
	}

	stream := &mockStream{
		chunks: []model.StreamChunk{},
		msg:    accMsg,
		usage:  model.Usage{InputTokens: 5, OutputTokens: 3},
	}

	reg := tool.NewRegistry()
	reg.Register(&mockTool{name: "search", result: "result here"})

	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	n := NewLLMNode("llm1", resolver, reg, LLMConfig{
		ToolNames: []string{"search"},
	})

	board := graph.NewBoard()
	board.SetChannel(workflow.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "find something"),
	})

	var events []graph.StreamEvent
	ctx := execCtxWithStream(func(ev graph.StreamEvent) {
		events = append(events, ev)
	})

	err := n.ExecuteBoard(ctx, board)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tp, _ := board.GetVar(VarToolPending)
	if tp != true {
		t.Fatalf("tool_pending = %v, want true", tp)
	}

	hasToolCall := false
	hasToolResult := false
	for _, ev := range events {
		if ev.Type == "tool_call" {
			hasToolCall = true
		}
		if ev.Type == "tool_result" {
			hasToolResult = true
		}
	}
	if !hasToolCall || !hasToolResult {
		t.Fatalf("expected tool_call and tool_result events, got %v", events)
	}
}

// --- truncate ---

func TestTruncate_Short(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Fatalf("truncate = %q", got)
	}
}

func TestTruncate_Long(t *testing.T) {
	if got := truncate("hello world", 5); got != "hello..." {
		t.Fatalf("truncate = %q", got)
	}
}

func TestTruncate_Exact(t *testing.T) {
	if got := truncate("hello", 5); got != "hello" {
		t.Fatalf("truncate = %q", got)
	}
}

// --- buildMessages: system prompt already exists ---

func TestBuildMessages_NoSystemDuplicate(t *testing.T) {
	n := NewLLMNode("n", nil, nil, LLMConfig{SystemPrompt: "be helpful"})
	board := graph.NewBoard()
	board.SetChannel(workflow.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleSystem, "existing system"),
		model.NewTextMessage(model.RoleUser, "hi"),
	})

	msgs := n.buildMessages(n.config, board, workflow.MainChannel, workflow.VarMessages)
	systemCount := 0
	for _, m := range msgs {
		if m.Role == model.RoleSystem {
			systemCount++
		}
	}
	if systemCount != 1 {
		t.Fatalf("expected 1 system message, got %d", systemCount)
	}
}

// --- buildMessages: reads from messages var when channel is empty ---

func TestBuildMessages_FallbackToVar(t *testing.T) {
	n := NewLLMNode("n", nil, nil, LLMConfig{})
	board := graph.NewBoard()
	board.SetVar(workflow.VarMessages, []model.Message{
		model.NewTextMessage(model.RoleUser, "from var"),
	})

	msgs := n.buildMessages(n.config, board, "empty_channel", workflow.VarMessages)
	if len(msgs) != 1 || msgs[0].Content() != "from var" {
		t.Fatalf("expected message from var, got %v", msgs)
	}
}
