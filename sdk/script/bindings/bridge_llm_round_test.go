package bindings

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// ---------------------------------------------------------------------------
// Test doubles for llm.LLMResolver, llm.LLM, llm.StreamMessage
// ---------------------------------------------------------------------------
//
// These doubles intentionally model the *contract* the round logic
// relies on: Resolver hands back an LLM, the LLM opens a stream, and
// the stream replays a scripted sequence of (chunk, message, usage,
// err) tuples on demand. We capture everything the round code passes
// in (model name, options, history) so individual tests can assert on
// the wiring as well as the produced roundResult.

type fakeStream struct {
	chunks   []model.StreamChunk
	final    model.Message
	usage    model.Usage
	streamEr error
	closeEr  error

	pos       int
	closed    bool
	closeCnt  int
	advanceCh func() // optional hook fired after each Next() returning true
}

func (f *fakeStream) Next() bool {
	if f.pos >= len(f.chunks) {
		return false
	}
	if f.advanceCh != nil {
		f.advanceCh()
	}
	f.pos++
	return true
}

func (f *fakeStream) Current() model.StreamChunk {
	if f.pos == 0 || f.pos > len(f.chunks) {
		return model.StreamChunk{}
	}
	return f.chunks[f.pos-1]
}

func (f *fakeStream) Err() error             { return f.streamEr }
func (f *fakeStream) Message() model.Message { return f.final }
func (f *fakeStream) Usage() model.Usage     { return f.usage }

func (f *fakeStream) Close() error {
	f.closed = true
	f.closeCnt++
	return f.closeEr
}

type fakeLLM struct {
	stream  *fakeStream
	openErr error
	gotMsgs []model.Message
	gotOpts *llm.GenerateOptions // captured snapshot of effective options
}

func (f *fakeLLM) Generate(_ context.Context, _ []model.Message, _ ...llm.GenerateOption) (model.Message, model.TokenUsage, error) {
	return model.Message{}, model.TokenUsage{}, errors.New("Generate not used by bridge")
}

func (f *fakeLLM) GenerateStream(_ context.Context, msgs []model.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	f.gotMsgs = msgs
	f.gotOpts = llm.ApplyOptions(opts...)
	if f.openErr != nil {
		return nil, f.openErr
	}
	return f.stream, nil
}

type fakeResolver struct {
	llm        llm.LLM
	resolveErr error
	gotModel   string
	calls      int
}

func (r *fakeResolver) Resolve(_ context.Context, modelStr string) (llm.LLM, error) {
	r.calls++
	r.gotModel = modelStr
	if r.resolveErr != nil {
		return nil, r.resolveErr
	}
	return r.llm, nil
}

func (r *fakeResolver) InvalidateCache(_ string) {}

// ---------------------------------------------------------------------------
// startRound — wiring + early-failure paths
// ---------------------------------------------------------------------------

func TestStartRound_NilResolver(t *testing.T) {
	_, err := startRound(context.Background(), nil, nil, "src", nil, roundOptions{Model: "m"})
	if err == nil {
		t.Fatal("nil resolver should produce an error")
	}
	if !strings.Contains(err.Error(), `"src"`) {
		t.Errorf("error should include source label, got: %v", err)
	}
}

func TestStartRound_ResolveError_Wrapped(t *testing.T) {
	res := &fakeResolver{resolveErr: errors.New("nope")}
	_, err := startRound(context.Background(), res, nil, "src", nil, roundOptions{Model: "m"})
	if err == nil {
		t.Fatal("resolver error should propagate")
	}
	if !strings.Contains(err.Error(), `"src"`) || !strings.Contains(err.Error(), `"m"`) {
		t.Errorf("error should include source and model labels, got: %v", err)
	}
	if !errors.Is(err, res.resolveErr) {
		t.Errorf("error should wrap resolver error, got: %v", err)
	}
}

func TestStartRound_OpenStreamError_Wrapped(t *testing.T) {
	res := &fakeResolver{llm: &fakeLLM{openErr: errors.New("provider down")}}
	_, err := startRound(context.Background(), res, nil, "src", nil, roundOptions{Model: "m"})
	if err == nil {
		t.Fatal("GenerateStream error should propagate")
	}
	if !strings.Contains(err.Error(), "open stream") {
		t.Errorf("error should mention open stream, got: %v", err)
	}
}

func TestStartRound_CapturesEffectiveOptions(t *testing.T) {
	temp := 0.25
	llmd := &fakeLLM{stream: &fakeStream{}}
	res := &fakeResolver{llm: llmd}

	reg := tool.NewRegistry()
	reg.Register(tool.FuncTool(
		model.ToolDefinition{Name: "search"},
		func(_ context.Context, _ string) (string, error) { return "", nil },
	))
	reg.Register(tool.FuncTool(
		model.ToolDefinition{Name: "calc"},
		func(_ context.Context, _ string) (string, error) { return "", nil },
	))

	_, err := startRound(context.Background(), res, reg, "src", nil, roundOptions{
		Model:       "m",
		Temperature: &temp,
		MaxTokens:   1024,
		JSONMode:    true,
		Thinking:    true,
		ToolNames:   []string{"search"}, // calc must be filtered out
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.gotModel != "m" {
		t.Errorf("model label not forwarded: %q", res.gotModel)
	}
	got := llmd.gotOpts
	if got.Temperature == nil || *got.Temperature != temp {
		t.Errorf("temperature not forwarded: %v", got.Temperature)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 1024 {
		t.Errorf("max_tokens not forwarded: %v", got.MaxTokens)
	}
	if got.JSONMode == nil || !*got.JSONMode {
		t.Errorf("json_mode not forwarded: %v", got.JSONMode)
	}
	if got.Thinking == nil || !*got.Thinking {
		t.Errorf("thinking not forwarded: %v", got.Thinking)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "search" {
		t.Errorf("tools should be filtered to allowed names, got: %+v", got.Tools)
	}
}

func TestStartRound_NoToolNames_NoToolsAdvertised(t *testing.T) {
	llmd := &fakeLLM{stream: &fakeStream{}}
	res := &fakeResolver{llm: llmd}

	reg := tool.NewRegistry()
	reg.Register(tool.FuncTool(
		model.ToolDefinition{Name: "anything"},
		func(_ context.Context, _ string) (string, error) { return "", nil },
	))

	_, err := startRound(context.Background(), res, reg, "src", nil, roundOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(llmd.gotOpts.Tools) != 0 {
		t.Errorf("tools should NOT be advertised when ToolNames is empty, got: %+v", llmd.gotOpts.Tools)
	}
}

func TestStartRound_DefensiveCopiesHistory(t *testing.T) {
	hist := []model.Message{model.NewTextMessage(model.RoleUser, "hi")}
	llmd := &fakeLLM{stream: &fakeStream{}}
	res := &fakeResolver{llm: llmd}

	rs, err := startRound(context.Background(), res, nil, "src", hist, roundOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	// Mutate the caller's slice; rs.history should have been copied.
	hist[0] = model.NewTextMessage(model.RoleUser, "MUTATED")
	if rs.history[0].Content() != "hi" {
		t.Errorf("rs.history aliased caller slice: %q", rs.history[0].Content())
	}
}

// ---------------------------------------------------------------------------
// Next / Current / Text — chunk projection
// ---------------------------------------------------------------------------

func TestRoundStream_TextChunks_ProjectAsPartText(t *testing.T) {
	stream := &fakeStream{
		chunks: []model.StreamChunk{
			{Content: "hel"},
			{Content: "lo"},
		},
		final: model.NewTextMessage(model.RoleAssistant, "hello"),
		usage: model.Usage{InputTokens: 1, OutputTokens: 2},
	}
	res := &fakeResolver{llm: &fakeLLM{stream: stream}}
	rs, err := startRound(context.Background(), res, nil, "src", nil, roundOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}

	var collected []string
	for rs.Next() {
		p := rs.Current()
		if p.Type != model.PartText {
			t.Fatalf("expected text part, got %q", p.Type)
		}
		if rs.Text() != p.Text {
			t.Errorf("Text() should mirror Current().Text, got %q vs %q", rs.Text(), p.Text)
		}
		collected = append(collected, p.Text)
	}
	if got := strings.Join(collected, ""); got != "hello" {
		t.Fatalf("text accumulation lost: %q", got)
	}
}

func TestRoundStream_ToolCallChunk_ProjectAsPartToolCall(t *testing.T) {
	stream := &fakeStream{
		chunks: []model.StreamChunk{
			{ToolCalls: []model.ToolCall{{ID: "c1", Name: "search", Arguments: `{"q":"go"}`}}},
		},
		final: model.NewToolCallMessage([]model.ToolCall{
			{ID: "c1", Name: "search", Arguments: `{"q":"go"}`},
		}),
	}
	res := &fakeResolver{llm: &fakeLLM{stream: stream}}
	rs, err := startRound(context.Background(), res, nil, "src", nil, roundOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}

	if !rs.Next() {
		t.Fatal("expected one chunk")
	}
	p := rs.Current()
	if p.Type != model.PartToolCall {
		t.Fatalf("expected tool_call part, got %q", p.Type)
	}
	if p.ToolCall == nil || p.ToolCall.ID != "c1" {
		t.Fatalf("tool_call payload lost: %#v", p.ToolCall)
	}
	if rs.Text() != "" {
		t.Errorf("Text() should be empty for non-text part, got %q", rs.Text())
	}
}

func TestRoundStream_Current_ZeroAfterDrain(t *testing.T) {
	stream := &fakeStream{chunks: []model.StreamChunk{{Content: "hi"}}}
	res := &fakeResolver{llm: &fakeLLM{stream: stream}}
	rs, _ := startRound(context.Background(), res, nil, "src", nil, roundOptions{Model: "m"})

	for rs.Next() {
	}
	if rs.Current() != (model.Part{}) {
		t.Errorf("Current should reset to zero Part after exhaustion, got %#v", rs.Current())
	}
}

// ---------------------------------------------------------------------------
// Finish — assembly + tool execution + close semantics
// ---------------------------------------------------------------------------

func TestFinish_TextOnly_ReturnsContentAndUsage(t *testing.T) {
	stream := &fakeStream{
		chunks: []model.StreamChunk{{Content: "hello"}},
		final:  model.NewTextMessage(model.RoleAssistant, "hello"),
		usage:  model.Usage{InputTokens: 4, OutputTokens: 6},
	}
	res := &fakeResolver{llm: &fakeLLM{stream: stream}}
	rs, _ := startRound(context.Background(), res, nil, "src", nil, roundOptions{Model: "m"})
	for rs.Next() {
	}
	r, err := rs.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if r.Content != "hello" {
		t.Errorf("content = %q", r.Content)
	}
	if r.Usage != (model.TokenUsage{InputTokens: 4, OutputTokens: 6, TotalTokens: 10}) {
		t.Errorf("usage = %+v", r.Usage)
	}
	if r.ToolPending {
		t.Error("text-only round should not mark tool_pending")
	}
	if !stream.closed {
		t.Error("stream must be closed after Finish")
	}
}

func TestFinish_SynthesizesAssistantWhenProviderEmpty(t *testing.T) {
	// Some providers stream tokens but do not bother building a final
	// Message. The bridge must reconstruct a text-only assistant
	// message from the accumulated text so downstream consumers always
	// get a usable Message.
	stream := &fakeStream{
		chunks: []model.StreamChunk{{Content: "hi "}, {Content: "there"}},
		final:  model.Message{}, // empty
	}
	res := &fakeResolver{llm: &fakeLLM{stream: stream}}
	rs, _ := startRound(context.Background(), res, nil, "src", nil, roundOptions{Model: "m"})
	for rs.Next() {
	}
	r, err := rs.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if r.Message.Role != model.RoleAssistant {
		t.Errorf("synthesized message should have assistant role, got %q", r.Message.Role)
	}
	if r.Message.Content() != "hi there" {
		t.Errorf("synthesized content = %q", r.Message.Content())
	}
	// Content() on the final result mirrors the synthesized message.
	if r.Content != "hi there" {
		t.Errorf("r.Content = %q", r.Content)
	}
}

func TestFinish_ToolCalls_ExecuteOnceAndAppendResult(t *testing.T) {
	calls := []model.ToolCall{
		{ID: "c1", Name: "echo", Arguments: `{"v":"a"}`},
	}
	stream := &fakeStream{
		chunks: nil,
		final:  model.NewToolCallMessage(calls),
	}

	reg := tool.NewRegistry()
	var execCount int
	reg.Register(tool.FuncTool(
		model.ToolDefinition{Name: "echo"},
		func(_ context.Context, args string) (string, error) {
			execCount++
			return "echoed:" + args, nil
		},
	))

	res := &fakeResolver{llm: &fakeLLM{stream: stream}}
	hist := []model.Message{model.NewTextMessage(model.RoleUser, "go")}
	rs, _ := startRound(context.Background(), res, reg, "src", hist, roundOptions{Model: "m"})
	for rs.Next() {
	}
	r, err := rs.Finish()
	if err != nil {
		t.Fatal(err)
	}

	if !r.ToolPending {
		t.Error("expected ToolPending=true when tools were executed")
	}
	if execCount != 1 {
		t.Errorf("tool should run exactly once per call (single round), ran %d times", execCount)
	}
	if len(r.ToolResults) != 1 || r.ToolResults[0].ToolCallID != "c1" {
		t.Errorf("tool results lost: %+v", r.ToolResults)
	}

	// Conversation tail: user history + assistant tool_call + tool result.
	if len(r.Messages) != 3 {
		t.Fatalf("expected 3 messages (user, assistant, tool), got %d: %+v", len(r.Messages), r.Messages)
	}
	if r.Messages[0].Role != model.RoleUser {
		t.Errorf("messages[0] role: %q", r.Messages[0].Role)
	}
	if r.Messages[1].Role != model.RoleAssistant {
		t.Errorf("messages[1] role: %q", r.Messages[1].Role)
	}
	if r.Messages[2].Role != model.RoleTool {
		t.Errorf("messages[2] role: %q", r.Messages[2].Role)
	}
}

func TestFinish_ToolCalls_NoRegistry_NoExecution(t *testing.T) {
	calls := []model.ToolCall{{ID: "c1", Name: "x"}}
	stream := &fakeStream{final: model.NewToolCallMessage(calls)}
	res := &fakeResolver{llm: &fakeLLM{stream: stream}}
	rs, _ := startRound(context.Background(), res, nil, "src", nil, roundOptions{Model: "m"})
	for rs.Next() {
	}
	r, err := rs.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if r.ToolPending {
		t.Error("ToolPending must stay false when registry is nil")
	}
	if len(r.ToolResults) != 0 {
		t.Errorf("no registry => no results, got %+v", r.ToolResults)
	}
	// Tool calls themselves still surface so the script can decide.
	if len(r.ToolCalls) != 1 {
		t.Errorf("ToolCalls should still be reported: %+v", r.ToolCalls)
	}
}

func TestFinish_StreamError_Surfaces(t *testing.T) {
	stream := &fakeStream{
		chunks:   []model.StreamChunk{{Content: "x"}},
		streamEr: errors.New("network reset"),
	}
	res := &fakeResolver{llm: &fakeLLM{stream: stream}}
	rs, _ := startRound(context.Background(), res, nil, "src", nil, roundOptions{Model: "m"})
	for rs.Next() {
	}
	_, err := rs.Finish()
	if err == nil {
		t.Fatal("stream error should be surfaced")
	}
	if !strings.Contains(err.Error(), "stream error") {
		t.Errorf("error should mention stream error, got: %v", err)
	}
	if !errors.Is(err, stream.streamEr) {
		t.Errorf("error should wrap stream.Err, got: %v", err)
	}
	// Even on error, Close must run (we promise no leaks).
	if !stream.closed {
		t.Error("stream must be closed even on error")
	}
}

func TestFinish_DoubleCall_SecondReturnsClosedError(t *testing.T) {
	stream := &fakeStream{
		chunks: []model.StreamChunk{{Content: "x"}},
		final:  model.NewTextMessage(model.RoleAssistant, "x"),
	}
	res := &fakeResolver{llm: &fakeLLM{stream: stream}}
	rs, _ := startRound(context.Background(), res, nil, "src", nil, roundOptions{Model: "m"})
	for rs.Next() {
	}
	if _, err := rs.Finish(); err != nil {
		t.Fatalf("first Finish: %v", err)
	}
	_, err := rs.Finish()
	if err == nil || !strings.Contains(err.Error(), "already closed") {
		t.Errorf("second Finish should error with 'already closed', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// runRound — synchronous shortcut
// ---------------------------------------------------------------------------

func TestRunRound_DrainsAndFinishes(t *testing.T) {
	stream := &fakeStream{
		chunks: []model.StreamChunk{{Content: "ab"}, {Content: "cd"}},
		final:  model.NewTextMessage(model.RoleAssistant, "abcd"),
		usage:  model.Usage{InputTokens: 1, OutputTokens: 2},
	}
	res := &fakeResolver{llm: &fakeLLM{stream: stream}}
	r, err := runRound(context.Background(), res, nil, "src", nil, roundOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Content != "abcd" {
		t.Errorf("content = %q", r.Content)
	}
	if r.Usage.TotalTokens != 3 {
		t.Errorf("usage total = %d", r.Usage.TotalTokens)
	}
	if !stream.closed {
		t.Error("runRound must close the stream")
	}
}

func TestRunRound_StartFailure_NoLeak(t *testing.T) {
	res := &fakeResolver{resolveErr: errors.New("nope")}
	_, err := runRound(context.Background(), res, nil, "src", nil, roundOptions{Model: "m"})
	if err == nil {
		t.Fatal("runRound should propagate startRound failure")
	}
	// Nothing to leak — the resolver never produced a stream — but
	// confirm the call count to make the contract explicit.
	if res.calls != 1 {
		t.Errorf("resolver should have been called exactly once, got %d", res.calls)
	}
}

// ---------------------------------------------------------------------------
// roundOptions.generateOptions / selectToolDefs — exhaustive unit tests
// ---------------------------------------------------------------------------

func TestRoundOptions_GenerateOptions_OmitsUnsetFields(t *testing.T) {
	got := llm.ApplyOptions(roundOptions{}.generateOptions(nil)...)
	if got.Temperature != nil || got.MaxTokens != nil || got.JSONMode != nil || got.Thinking != nil {
		t.Errorf("zero roundOptions should produce zero GenerateOptions, got %+v", got)
	}
	if len(got.Tools) != 0 {
		t.Errorf("no tools should be advertised, got %+v", got.Tools)
	}
}

func TestSelectToolDefs_Filters(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(tool.FuncTool(model.ToolDefinition{Name: "a"}, func(_ context.Context, _ string) (string, error) { return "", nil }))
	reg.Register(tool.FuncTool(model.ToolDefinition{Name: "b"}, func(_ context.Context, _ string) (string, error) { return "", nil }))
	reg.Register(tool.FuncTool(model.ToolDefinition{Name: "c"}, func(_ context.Context, _ string) (string, error) { return "", nil }))

	defs := selectToolDefs(reg, []string{"a", "c", "missing"})
	names := make(map[string]bool, len(defs))
	for _, d := range defs {
		names[d.Name] = true
	}
	if len(defs) != 2 || !names["a"] || !names["c"] {
		t.Fatalf("expected only a + c, got %+v", defs)
	}
}

func TestSelectToolDefs_NilRegistry_ReturnsNil(t *testing.T) {
	if defs := selectToolDefs(nil, []string{"a"}); defs != nil {
		t.Fatalf("nil registry should produce nil, got %+v", defs)
	}
}

func TestSelectToolDefs_EmptyNames_ReturnsNil(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(tool.FuncTool(model.ToolDefinition{Name: "a"}, func(_ context.Context, _ string) (string, error) { return "", nil }))
	if defs := selectToolDefs(reg, nil); defs != nil {
		t.Fatalf("empty names should produce nil, got %+v", defs)
	}
}
