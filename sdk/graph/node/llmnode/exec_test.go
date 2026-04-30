package llmnode

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
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
func (s *mockStream) Current() model.StreamChunk { return s.chunks[s.idx-1] }
func (s *mockStream) Err() error                 { return s.errOnce }
func (s *mockStream) Close() error               { return nil }
func (s *mockStream) Message() model.Message     { return s.msg }
func (s *mockStream) Usage() model.Usage         { return s.usage }

type streamOnlyLLM struct{ stream llm.StreamMessage }

func (s *streamOnlyLLM) Generate(_ context.Context, _ []model.Message, _ ...llm.GenerateOption) (model.Message, model.TokenUsage, error) {
	return model.Message{}, model.TokenUsage{}, nil
}
func (s *streamOnlyLLM) GenerateStream(_ context.Context, _ []model.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return s.stream, nil
}

type mockResolver struct {
	llmInst llm.LLM
	err     error
}

func (r *mockResolver) Resolve(_ context.Context, _ string) (llm.LLM, error) {
	return r.llmInst, r.err
}
func (r *mockResolver) InvalidateCache(_ ...llm.InvalidateOption) {}

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

func execCtx() graph.ExecutionContext {
	return graph.ExecutionContext{Context: context.Background()}
}

// capturedEvent mirrors what the executor's publisher would deliver to a
// subscriber: the routed event type plus its payload. Tests use this
// struct so per-event assertions stay readable.
type capturedEvent struct {
	Type    string
	Payload any
}

func execCtxWithPublisher(events *[]capturedEvent) graph.ExecutionContext {
	return graph.ExecutionContext{
		Context: context.Background(),
		Publisher: graph.StreamPublisherFunc(func(t string, p any) {
			*events = append(*events, capturedEvent{Type: t, Payload: p})
		}),
	}
}

// --- ExecuteBoard ---

func TestNode_ExecuteBoard_Basic(t *testing.T) {
	resolver := &mockResolver{llmInst: &mockLLM{
		msg:   model.NewTextMessage(model.RoleAssistant, "hello world"),
		usage: model.Usage{InputTokens: 10, OutputTokens: 5},
	}}
	n := New("llm1", resolver, nil, Config{SystemPrompt: "be helpful"})

	board := graph.NewBoard()
	board.SetChannel(graph.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "hi"),
	})

	if err := n.ExecuteBoard(execCtx(), board); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := board.GetVar(VarResponse); !ok {
		t.Fatal("response var not set")
	}
	if tp, _ := board.GetVar(VarToolPending); tp != false {
		t.Fatalf("tool_pending = %v, want false", tp)
	}
}

func TestNode_ExecuteBoard_WithChunks(t *testing.T) {
	stream := &mockStream{
		chunks: []model.StreamChunk{{Content: "hello "}, {Content: "world"}},
		usage:  model.Usage{InputTokens: 5, OutputTokens: 3},
	}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	n := New("llm1", resolver, nil, Config{})

	board := graph.NewBoard()
	board.SetChannel(graph.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "test"),
	})

	var events []capturedEvent
	ctx := execCtxWithPublisher(&events)

	if err := n.ExecuteBoard(ctx, board); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var tokens []string
	for _, ev := range events {
		if ev.Type != "token" {
			continue
		}
		if p, ok := ev.Payload.(map[string]any); ok {
			tokens = append(tokens, p["content"].(string))
		}
	}
	if len(tokens) != 2 || tokens[0] != "hello " || tokens[1] != "world" {
		t.Fatalf("tokens = %v", tokens)
	}
	if resp, _ := board.GetVar(VarResponse); resp != "hello world" {
		t.Fatalf("response = %q", resp)
	}
}

func TestNode_ExecuteBoard_ResolveError(t *testing.T) {
	resolver := &mockResolver{err: errors.New("no model")}
	n := New("llm1", resolver, nil, Config{Model: "bad"})

	board := graph.NewBoard()
	board.SetChannel(graph.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "hi"),
	})

	if err := n.ExecuteBoard(execCtx(), board); err == nil {
		t.Fatal("expected error")
	}
}

func TestNode_ExecuteBoard_JSONMode(t *testing.T) {
	stream := &mockStream{chunks: []model.StreamChunk{{Content: `{"intent": "qa"}`}}}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	n := New("llm1", resolver, nil, Config{JSONMode: true, OutputKey: "result"})

	board := graph.NewBoard()
	board.SetChannel(graph.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "classify"),
	})

	if err := n.ExecuteBoard(execCtx(), board); err != nil {
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

func TestNode_ExecuteBoard_JSONMode_InvalidJSON(t *testing.T) {
	stream := &mockStream{chunks: []model.StreamChunk{{Content: "not json at all"}}}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	n := New("llm1", resolver, nil, Config{JSONMode: true, OutputKey: "result"})

	board := graph.NewBoard()
	board.SetVar("result", "pre-existing")
	board.SetChannel(graph.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "bad"),
	})

	if err := n.ExecuteBoard(execCtx(), board); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val, _ := board.GetVar("result"); val != "pre-existing" {
		t.Fatalf("expected pre-existing value preserved, got %v", val)
	}
}

func TestNode_ExecuteBoard_QueryFallback(t *testing.T) {
	stream := &mockStream{chunks: []model.StreamChunk{{Content: "resp"}}}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	n := New("llm1", resolver, nil, Config{
		MessagesKey:   "alt_msgs",
		QueryFallback: true,
	})

	board := graph.NewBoard()
	board.SetVar(graph.VarQuery, "what is this?")

	if err := n.ExecuteBoard(execCtx(), board); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNode_ExecuteBoard_TrackSteps(t *testing.T) {
	stream := &mockStream{chunks: []model.StreamChunk{{Content: "step1"}}}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	n := New("llm1", resolver, nil, Config{TrackSteps: true})

	board := graph.NewBoard()
	board.SetChannel(graph.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "hi"),
	})

	if err := n.ExecuteBoard(execCtx(), board); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	steps, ok := board.GetVar("agent_steps")
	if !ok {
		t.Fatal("agent_steps not set")
	}
	if len(steps.([]map[string]any)) != 1 {
		t.Fatalf("expected 1 step, got %d", len(steps.([]map[string]any)))
	}
}

func TestNode_ExecuteBoard_AccumulateUsage(t *testing.T) {
	stream := &mockStream{
		chunks: []model.StreamChunk{{Content: "x"}},
		usage:  model.Usage{InputTokens: 10, OutputTokens: 5},
	}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	n := New("llm1", resolver, nil, Config{})

	board := graph.NewBoard()
	board.SetVar(VarInternalUsage, model.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150})
	board.SetChannel(graph.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "hi"),
	})

	if err := n.ExecuteBoard(execCtx(), board); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	u, _ := board.GetVar(VarInternalUsage)
	if u.(model.TokenUsage).InputTokens != 110 {
		t.Fatalf("accumulated InputTokens = %d, want 110", u.(model.TokenUsage).InputTokens)
	}
}

func TestNode_ExecuteBoard_WithToolCalls(t *testing.T) {
	tc := model.ToolCall{ID: "tc1", Name: "search", Arguments: `{"q":"hello"}`}
	accMsg := model.Message{
		Role: model.RoleAssistant,
		Parts: []model.Part{
			{Type: model.PartToolCall, ToolCall: &tc},
		},
	}
	stream := &mockStream{msg: accMsg, usage: model.Usage{InputTokens: 5, OutputTokens: 3}}

	reg := tool.NewRegistry()
	reg.Register(&mockTool{name: "search", result: "result here"})

	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	n := New("llm1", resolver, reg, Config{ToolNames: []string{"search"}})

	board := graph.NewBoard()
	board.SetChannel(graph.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "find something"),
	})

	var events []capturedEvent
	ctx := execCtxWithPublisher(&events)

	if err := n.ExecuteBoard(ctx, board); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tp, _ := board.GetVar(VarToolPending); tp != true {
		t.Fatalf("tool_pending = %v, want true", tp)
	}

	hasToolCall, hasToolResult := false, false
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
