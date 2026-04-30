package llm

import (
	"context"
	"testing"
)

// defaultsCaptureLLM captures the merged options that reach the
// inner provider so tests can assert what defaults+caller produced.
type defaultsCaptureLLM struct {
	captured *GenerateOptions
}

func (m *defaultsCaptureLLM) Generate(_ context.Context, _ []Message, opts ...GenerateOption) (Message, TokenUsage, error) {
	m.captured = ApplyOptions(opts...)
	return NewTextMessage(RoleAssistant, "ok"), TokenUsage{}, nil
}

func (m *defaultsCaptureLLM) GenerateStream(_ context.Context, _ []Message, opts ...GenerateOption) (StreamMessage, error) {
	m.captured = ApplyOptions(opts...)
	return nil, nil
}

func TestWithDefaults_ZeroDefaults_ReturnsInner(t *testing.T) {
	inner := &defaultsCaptureLLM{}
	got := WithDefaults(inner, GenerateOptions{})
	if got != inner {
		t.Fatal("zero defaults should return inner unwrapped")
	}
}

func TestWithDefaults_FillsUnsetFields(t *testing.T) {
	inner := &defaultsCaptureLLM{}
	wrapped := WithDefaults(inner, GenerateOptions{
		Temperature: floatPtr(0.7),
		MaxTokens:   int64Ptr(500),
	})
	_, _, _ = wrapped.Generate(context.Background(), nil)
	if inner.captured.Temperature == nil || *inner.captured.Temperature != 0.7 {
		t.Errorf("default Temperature not applied: %v", inner.captured.Temperature)
	}
	if inner.captured.MaxTokens == nil || *inner.captured.MaxTokens != 500 {
		t.Errorf("default MaxTokens not applied: %v", inner.captured.MaxTokens)
	}
}

func TestWithDefaults_CallerWins(t *testing.T) {
	// Caller passes explicit Temperature; defaults must NOT override it.
	inner := &defaultsCaptureLLM{}
	wrapped := WithDefaults(inner, GenerateOptions{Temperature: floatPtr(0.7)})
	_, _, _ = wrapped.Generate(context.Background(), nil, WithTemperature(0.1))
	if *inner.captured.Temperature != 0.1 {
		t.Fatalf("caller Temperature should win: got %v", *inner.captured.Temperature)
	}
}

func TestWithDefaults_FillsExtraKeyWise(t *testing.T) {
	inner := &defaultsCaptureLLM{}
	wrapped := WithDefaults(inner, GenerateOptions{Extra: map[string]any{"region": "us-east", "model_alias": "x"}})
	_, _, _ = wrapped.Generate(context.Background(), nil, func(o *GenerateOptions) {
		o.Extra = map[string]any{"region": "eu-west"} // caller overrides one key
	})
	if inner.captured.Extra["region"] != "eu-west" {
		t.Errorf("caller key should win: %v", inner.captured.Extra["region"])
	}
	if inner.captured.Extra["model_alias"] != "x" {
		t.Errorf("default key should fill: %v", inner.captured.Extra["model_alias"])
	}
}
