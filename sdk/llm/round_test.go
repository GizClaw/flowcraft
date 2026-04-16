package llm

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// ── mocks for RunRound / StreamRound ──

type roundResolver struct{ llm LLM }

func (r *roundResolver) Resolve(_ context.Context, _ string) (LLM, error) {
	return r.llm, nil
}

func (r *roundResolver) InvalidateCache(_ string) {}

type failResolver struct{ err error }

func (r *failResolver) Resolve(_ context.Context, _ string) (LLM, error) {
	return nil, r.err
}

func (r *failResolver) InvalidateCache(_ string) {}

type roundFakeStream struct {
	chunks  []string
	idx     int
	err     error
	closed  atomic.Int32
}

func (s *roundFakeStream) Next() bool {
	if s.idx < len(s.chunks) {
		s.idx++
		return true
	}
	return false
}

func (s *roundFakeStream) Current() StreamChunk {
	return StreamChunk{Role: RoleAssistant, Content: s.chunks[s.idx-1]}
}

func (s *roundFakeStream) Err() error   { return s.err }
func (s *roundFakeStream) Close() error { s.closed.Add(1); return nil }
func (s *roundFakeStream) Message() Message {
	var b string
	for _, c := range s.chunks {
		b += c
	}
	return NewTextMessage(RoleAssistant, b)
}
func (s *roundFakeStream) Usage() Usage { return Usage{InputTokens: 10, OutputTokens: 5} }

type roundStreamLLM struct {
	stream *roundFakeStream
}

func (m *roundStreamLLM) Generate(_ context.Context, _ []Message, _ ...GenerateOption) (Message, TokenUsage, error) {
	return Message{}, TokenUsage{}, fmt.Errorf("not implemented")
}

func (m *roundStreamLLM) GenerateStream(_ context.Context, _ []Message, _ ...GenerateOption) (StreamMessage, error) {
	return m.stream, nil
}

func ptrFloat(v float64) *float64 { return &v }

func TestBuildRoundGenerateOptions_AllFlags(t *testing.T) {
	cfg := RoundConfig{
		Temperature: ptrFloat(0.8),
		MaxTokens:   100,
		Thinking:    true,
		JSONMode:    true,
	}
	opts := buildRoundGenerateOptions(cfg, nil)
	if len(opts) != 4 {
		t.Fatalf("expected 4 options, got %d", len(opts))
	}
}

func TestBuildRoundGenerateOptions_TemperatureZero(t *testing.T) {
	cfg := RoundConfig{Temperature: ptrFloat(0)}
	opts := buildRoundGenerateOptions(cfg, nil)
	if len(opts) != 1 {
		t.Fatalf("expected 1 option for explicit temperature=0, got %d", len(opts))
	}
}

type roundMockTool struct {
	name string
}

func (m *roundMockTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: m.name, Description: m.name}
}

func (m *roundMockTool) Execute(_ context.Context, _ string) (string, error) {
	return "ok", nil
}

func TestBuildRoundGenerateOptions_WithTools(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&roundMockTool{name: "a"})
	reg.Register(&roundMockTool{name: "b"})
	cfg := RoundConfig{ToolNames: []string{"a"}}
	opts := buildRoundGenerateOptions(cfg, reg)
	if len(opts) != 1 {
		t.Fatalf("expected 1 option (WithTools), got %d", len(opts))
	}
}

func TestBuildRoundGenerateOptions_NoOptions(t *testing.T) {
	opts := buildRoundGenerateOptions(RoundConfig{}, nil)
	if len(opts) != 0 {
		t.Fatalf("expected 0 options, got %d", len(opts))
	}
}

// ── RunRound tests ──

func TestRunRound_SimpleResponse(t *testing.T) {
	stream := &roundFakeStream{chunks: []string{"hello", " world"}}
	resolver := &roundResolver{llm: &roundStreamLLM{stream: stream}}
	msgs := []Message{NewTextMessage(RoleUser, "hi")}

	result, err := RunRound(context.Background(), nil, resolver, nil, "test", msgs, RoundConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Content != "hello world" {
		t.Fatalf("Content = %q, want %q", result.Content, "hello world")
	}
	if result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 5 {
		t.Fatalf("Usage = %+v", result.Usage)
	}
	if result.ToolPending {
		t.Fatal("ToolPending should be false")
	}
	if len(result.Messages) != 2 {
		t.Fatalf("Messages len = %d, want 2 (input + assistant)", len(result.Messages))
	}
}

func TestRunRound_ResolveError(t *testing.T) {
	resolver := &failResolver{err: fmt.Errorf("no such model")}
	_, err := RunRound(context.Background(), nil, resolver, nil, "test", nil, RoundConfig{Model: "bad"})
	if err == nil {
		t.Fatal("expected error from resolve failure")
	}
}

func TestRunRound_StreamCallbackReceivesTokens(t *testing.T) {
	stream := &roundFakeStream{chunks: []string{"a", "b", "c"}}
	resolver := &roundResolver{llm: &roundStreamLLM{stream: stream}}

	var tokens []string
	cb := func(ev workflow.StreamEvent) {
		if ev.Type == "token" {
			if p, ok := ev.Payload.(map[string]any); ok {
				if c, ok := p["content"].(string); ok {
					tokens = append(tokens, c)
				}
			}
		}
	}

	_, err := RunRound(context.Background(), cb, resolver, nil, "n1", nil, RoundConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tokens) != 3 || tokens[0] != "a" || tokens[1] != "b" || tokens[2] != "c" {
		t.Fatalf("tokens = %v", tokens)
	}
}

func TestRunRound_StreamError(t *testing.T) {
	stream := &roundFakeStream{chunks: []string{"partial"}, err: fmt.Errorf("connection reset")}
	resolver := &roundResolver{llm: &roundStreamLLM{stream: stream}}

	_, err := RunRound(context.Background(), nil, resolver, nil, "test", nil, RoundConfig{})
	if err == nil {
		t.Fatal("expected error from stream failure")
	}
}

// ── StreamRound tests ──

func TestStreamRound_ManualIteration(t *testing.T) {
	stream := &roundFakeStream{chunks: []string{"x", "y"}}
	resolver := &roundResolver{llm: &roundStreamLLM{stream: stream}}

	s, err := StreamRound(context.Background(), nil, resolver, nil, "test", nil, RoundConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer s.Close()

	var collected []string
	for s.Next() {
		collected = append(collected, s.Token())
	}
	if len(collected) != 2 || collected[0] != "x" || collected[1] != "y" {
		t.Fatalf("collected = %v", collected)
	}

	result, err := s.Finish()
	if err != nil {
		t.Fatalf("Finish error: %v", err)
	}
	if result.Content != "xy" {
		t.Fatalf("Content = %q, want %q", result.Content, "xy")
	}
}

// ── Close idempotency ──

func TestRoundStream_CloseIdempotent(t *testing.T) {
	stream := &roundFakeStream{chunks: []string{"hi"}}
	resolver := &roundResolver{llm: &roundStreamLLM{stream: stream}}

	s, err := StreamRound(context.Background(), nil, resolver, nil, "test", nil, RoundConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for s.Next() {
	}

	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if n := stream.closed.Load(); n != 1 {
		t.Fatalf("inner.Close called %d times, want 1", n)
	}
}

func TestRunRound_CloseCalledExactlyOnce(t *testing.T) {
	stream := &roundFakeStream{chunks: []string{"ok"}}
	resolver := &roundResolver{llm: &roundStreamLLM{stream: stream}}

	_, err := RunRound(context.Background(), nil, resolver, nil, "test", nil, RoundConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := stream.closed.Load(); n != 1 {
		t.Fatalf("inner.Close called %d times, want exactly 1", n)
	}
}

// ── RoundConfigFromMap tests ──

func TestRoundConfigFromMap_Valid(t *testing.T) {
	cfg, err := RoundConfigFromMap(map[string]any{
		"model":       "gpt-4",
		"temperature": 0.5,
		"max_tokens":  float64(100),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Model != "gpt-4" {
		t.Fatalf("Model = %q", cfg.Model)
	}
	if cfg.Temperature == nil || *cfg.Temperature != 0.5 {
		t.Fatalf("Temperature = %v", cfg.Temperature)
	}
	if cfg.MaxTokens != 100 {
		t.Fatalf("MaxTokens = %d", cfg.MaxTokens)
	}
}

func TestRoundConfigFromMap_Nil(t *testing.T) {
	cfg, err := RoundConfigFromMap(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Model != "" {
		t.Fatalf("nil map should produce zero-value config")
	}
}

func TestRoundConfigFromMap_TemperatureZero(t *testing.T) {
	cfg, err := RoundConfigFromMap(map[string]any{"temperature": 0.0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Temperature == nil {
		t.Fatal("explicit temperature=0 should produce non-nil pointer")
	}
	if *cfg.Temperature != 0 {
		t.Fatalf("Temperature = %v, want 0", *cfg.Temperature)
	}
}
