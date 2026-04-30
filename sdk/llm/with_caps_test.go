package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// ---------------------------------------------------------------------------
// Mocks
// ---------------------------------------------------------------------------

type capsMockLLM struct {
	lastOpts *GenerateOptions
}

func (m *capsMockLLM) Generate(_ context.Context, _ []Message, opts ...GenerateOption) (Message, TokenUsage, error) {
	m.lastOpts = ApplyOptions(opts...)
	return NewTextMessage(RoleAssistant, "ok"), TokenUsage{}, nil
}

func (m *capsMockLLM) GenerateStream(_ context.Context, _ []Message, opts ...GenerateOption) (StreamMessage, error) {
	m.lastOpts = ApplyOptions(opts...)
	return nil, nil
}

// foldCaptureLLM records the message slice the caps middleware passed
// downstream. Used by system-prompt-fold tests where the assertion is
// on the transformed Messages, not on opts.
type foldCaptureLLM struct {
	msgs []Message
}

func (f *foldCaptureLLM) Generate(_ context.Context, msgs []Message, _ ...GenerateOption) (Message, TokenUsage, error) {
	f.msgs = msgs
	return NewTextMessage(RoleAssistant, "ok"), TokenUsage{}, nil
}

func (f *foldCaptureLLM) GenerateStream(_ context.Context, msgs []Message, _ ...GenerateOption) (StreamMessage, error) {
	f.msgs = msgs
	return nil, nil
}

// ---------------------------------------------------------------------------
// Zero-caps short-circuit
// ---------------------------------------------------------------------------

func TestWithCaps_ZeroCaps_NoWrap(t *testing.T) {
	inner := &capsMockLLM{}
	if WithCaps(inner, ModelCaps{}) != inner {
		t.Fatal("zero-value caps should return inner as-is")
	}
}

// ---------------------------------------------------------------------------
// Generation-param drops
// ---------------------------------------------------------------------------

func TestWithCaps_DisableTemperature(t *testing.T) {
	inner := &capsMockLLM{}
	wrapped := WithCaps(inner, DisabledCaps(CapTemperature))
	_, _, _ = wrapped.Generate(context.Background(), nil, WithTemperature(0.8))
	if inner.lastOpts.Temperature != nil {
		t.Fatalf("expected temperature=nil after caps filter, got %v", *inner.lastOpts.Temperature)
	}
}

func TestWithCaps_DropsTopP_TopK_StopWords(t *testing.T) {
	inner := &capsMockLLM{}
	wrapped := WithCaps(inner, DisabledCaps(CapTopP, CapTopK, CapStopWords))
	_, _, _ = wrapped.Generate(context.Background(), nil,
		WithTopP(0.9), WithTopK(50), WithStopWords("STOP"))
	if inner.lastOpts.TopP != nil {
		t.Errorf("TopP not dropped: %v", *inner.lastOpts.TopP)
	}
	if inner.lastOpts.TopK != nil {
		t.Errorf("TopK not dropped: %v", *inner.lastOpts.TopK)
	}
	if len(inner.lastOpts.StopWords) != 0 {
		t.Errorf("StopWords not dropped: %v", inner.lastOpts.StopWords)
	}
}

// ---------------------------------------------------------------------------
// JSON schema / mode interaction
// ---------------------------------------------------------------------------

func TestWithCaps_DisableJSONSchema_Downgrade(t *testing.T) {
	inner := &capsMockLLM{}
	wrapped := WithCaps(inner, DisabledCaps(CapJSONSchema))
	schema := JSONSchemaParam{Name: "test", Schema: map[string]any{"type": "object"}}
	_, _, _ = wrapped.Generate(context.Background(), nil, WithJSONSchema(schema))
	if inner.lastOpts.JSONSchema != nil {
		t.Fatal("expected JSONSchema=nil after disabling CapJSONSchema")
	}
	if inner.lastOpts.JSONMode == nil || !*inner.lastOpts.JSONMode {
		t.Fatal("expected JSONMode=true after CapJSONSchema downgrade")
	}
}

func TestWithCaps_DisableJSONSchema_NoopWithoutSchema(t *testing.T) {
	inner := &capsMockLLM{}
	wrapped := WithCaps(inner, DisabledCaps(CapJSONSchema))
	_, _, _ = wrapped.Generate(context.Background(), nil, WithTemperature(0.5))
	if inner.lastOpts.JSONMode != nil {
		t.Fatal("downgrade should not set JSONMode when JSONSchema was not set")
	}
}

func TestWithCaps_DisableJSONMode(t *testing.T) {
	inner := &capsMockLLM{}
	wrapped := WithCaps(inner, DisabledCaps(CapJSONMode))
	_, _, _ = wrapped.Generate(context.Background(), nil, WithJSONMode(true))
	if inner.lastOpts.JSONMode != nil {
		t.Fatal("expected JSONMode=nil after disabling CapJSONMode")
	}
}

func TestWithCaps_AllJSONCapsDisabled_BothCleared(t *testing.T) {
	inner := &capsMockLLM{}
	wrapped := WithCaps(inner, DisabledCaps(CapTemperature, CapJSONSchema, CapJSONMode))
	schema := JSONSchemaParam{Name: "test", Schema: map[string]any{"type": "object"}}
	_, _, _ = wrapped.Generate(context.Background(), nil,
		WithTemperature(0.8), WithJSONSchema(schema), WithJSONMode(true))
	if inner.lastOpts.Temperature != nil {
		t.Fatal("expected temperature cleared")
	}
	if inner.lastOpts.JSONSchema != nil {
		t.Fatal("expected JSONSchema cleared")
	}
	if inner.lastOpts.JSONMode != nil {
		t.Fatal("expected JSONMode cleared (CapJSONMode overrides downgrade)")
	}
}

// ---------------------------------------------------------------------------
// Tools cap
// ---------------------------------------------------------------------------

func TestWithCaps_DropsTools(t *testing.T) {
	inner := &capsMockLLM{}
	wrapped := WithCaps(inner, DisabledCaps(CapTools))
	_, _, _ = wrapped.Generate(context.Background(), nil,
		WithTools(ToolDefinition{Name: "x"}))
	if len(inner.lastOpts.Tools) != 0 {
		t.Fatalf("Tools should be dropped when CapTools disabled")
	}
}

// ---------------------------------------------------------------------------
// Stream path & streaming downgrade
// ---------------------------------------------------------------------------

func TestWithCaps_StreamPath_AppliesDrops(t *testing.T) {
	inner := &capsMockLLM{}
	wrapped := WithCaps(inner, DisabledCaps(CapTemperature))
	_, _ = wrapped.GenerateStream(context.Background(), nil, WithTemperature(0.8))
	if inner.lastOpts.Temperature != nil {
		t.Fatal("expected temperature cleared in stream path")
	}
}

func TestWithCaps_StreamingDowngrade_OneChunk(t *testing.T) {
	inner := &capsMockLLM{}
	wrapped := WithCaps(inner, DisabledCaps(CapStreaming))
	stream, err := wrapped.GenerateStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Next() {
		t.Fatal("first Next should yield the synthesized chunk")
	}
	chunk := stream.Current()
	if chunk.Content != "ok" {
		t.Errorf("expected chunk Content 'ok', got %q", chunk.Content)
	}
	if chunk.FinishReason != "stop" {
		t.Errorf("expected FinishReason 'stop', got %q", chunk.FinishReason)
	}
	if stream.Next() {
		t.Fatal("second Next must return false")
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("unexpected Err: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close returned: %v", err)
	}
}

// ---------------------------------------------------------------------------
// System-prompt fold
// ---------------------------------------------------------------------------

func TestWithCaps_SystemPromptFold_PrependsToFirstUser(t *testing.T) {
	captured := &foldCaptureLLM{}
	wrapped := WithCaps(captured, DisabledCaps(CapSystemPrompt))
	msgs := []Message{
		{Role: model.RoleSystem, Parts: []model.Part{{Type: model.PartText, Text: "be terse"}}},
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "hi"}}},
	}
	_, _, _ = wrapped.Generate(context.Background(), msgs)
	if got := len(captured.msgs); got != 1 {
		t.Fatalf("expected system message removed, got %d msgs", got)
	}
	if captured.msgs[0].Role != model.RoleUser {
		t.Errorf("first surviving message should be user, got %s", captured.msgs[0].Role)
	}
	text := captured.msgs[0].Content()
	if !strings.HasPrefix(text, "[System: be terse]") || !strings.HasSuffix(text, "hi") {
		t.Errorf("fold format wrong: %q", text)
	}
}

func TestWithCaps_SystemPromptFold_NoUser_SynthesizesOne(t *testing.T) {
	captured := &foldCaptureLLM{}
	wrapped := WithCaps(captured, DisabledCaps(CapSystemPrompt))
	msgs := []Message{
		{Role: model.RoleSystem, Parts: []model.Part{{Type: model.PartText, Text: "rules"}}},
	}
	_, _, _ = wrapped.Generate(context.Background(), msgs)
	if len(captured.msgs) != 1 || captured.msgs[0].Role != model.RoleUser {
		t.Fatalf("expected synthesized user message; got %+v", captured.msgs)
	}
	if !strings.Contains(captured.msgs[0].Content(), "rules") {
		t.Errorf("synthesized user should carry system text; got %q", captured.msgs[0].Content())
	}
}

// ---------------------------------------------------------------------------
// Modality validation
// ---------------------------------------------------------------------------

func TestWithCaps_VisionDisabled_RejectsImagePart(t *testing.T) {
	wrapped := WithCaps(&capsMockLLM{}, DisabledCaps(CapVision))
	msgs := []Message{
		{Role: model.RoleUser, Parts: []model.Part{
			{Type: model.PartImage, Image: &model.MediaRef{URL: "http://x/y.png"}},
		}},
	}
	_, _, err := wrapped.Generate(context.Background(), msgs)
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error, got %v", err)
	}
}

func TestWithCaps_AudioDisabled_RejectsAudioPart(t *testing.T) {
	wrapped := WithCaps(&capsMockLLM{}, DisabledCaps(CapAudio))
	msgs := []Message{
		{Role: model.RoleUser, Parts: []model.Part{
			{Type: model.PartAudio, Audio: &model.MediaRef{URL: "http://x/y.mp3"}},
		}},
	}
	_, _, err := wrapped.Generate(context.Background(), msgs)
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error, got %v", err)
	}
}

func TestWithCaps_ParallelToolsDisabled_StripsKnownExtras(t *testing.T) {
	// Disabling CapParallelTools must remove the protocol-reserved
	// Extra keys so adapters never forward a parallel-tool toggle to
	// the backend. Other Extra keys must be preserved untouched.
	mock := &capsMockLLM{}
	wrapped := WithCaps(mock, DisabledCaps(CapParallelTools))

	_, _, err := wrapped.Generate(context.Background(),
		[]Message{NewTextMessage(model.RoleUser, "hi")},
		WithExtra("parallel_tool_calls", false),
		WithExtra("disable_parallel_tool_use", true),
		WithExtra("unrelated", "keep-me"),
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if mock.lastOpts == nil {
		t.Fatal("mock did not capture opts")
	}
	if _, ok := mock.lastOpts.Extra["parallel_tool_calls"]; ok {
		t.Error("parallel_tool_calls should have been stripped")
	}
	if _, ok := mock.lastOpts.Extra["disable_parallel_tool_use"]; ok {
		t.Error("disable_parallel_tool_use should have been stripped")
	}
	if got, ok := mock.lastOpts.Extra["unrelated"]; !ok || got != "keep-me" {
		t.Errorf("unrelated key was modified or dropped: got %v ok=%v", got, ok)
	}
}

func TestWithCaps_ParallelToolsEnabled_PreservesExtras(t *testing.T) {
	// Sanity: when the cap is supported the Extra map must pass
	// through verbatim. Catches accidental unconditional stripping.
	mock := &capsMockLLM{}
	wrapped := WithCaps(mock, DisabledCaps(CapTemperature)) // unrelated cap disabled

	_, _, err := wrapped.Generate(context.Background(),
		[]Message{NewTextMessage(model.RoleUser, "hi")},
		WithExtra("parallel_tool_calls", false),
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got, ok := mock.lastOpts.Extra["parallel_tool_calls"]; !ok || got != false {
		t.Errorf("parallel_tool_calls should have survived: got %v ok=%v", got, ok)
	}
}

func TestRegisterParallelToolExtraKey_PicksUpCustomKey(t *testing.T) {
	// New adapter registers a custom toggle name → middleware must
	// strip it under CapParallelTools=disabled just like the built-ins.
	RegisterParallelToolExtraKey("custom_parallel_toggle")

	mock := &capsMockLLM{}
	wrapped := WithCaps(mock, DisabledCaps(CapParallelTools))
	_, _, err := wrapped.Generate(context.Background(),
		[]Message{NewTextMessage(model.RoleUser, "hi")},
		WithExtra("custom_parallel_toggle", "off"),
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, ok := mock.lastOpts.Extra["custom_parallel_toggle"]; ok {
		t.Error("custom registered key should have been stripped")
	}
}
