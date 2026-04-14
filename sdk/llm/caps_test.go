package llm

import (
	"context"
	"testing"
)

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

func TestCapsMiddleware_ZeroCaps_NoWrap(t *testing.T) {
	inner := &capsMockLLM{}
	wrapped := CapsMiddleware(inner, ModelCaps{})
	if wrapped != inner {
		t.Fatal("zero-value caps should return inner as-is")
	}
}

func TestCapsMiddleware_DisableTemperature(t *testing.T) {
	inner := &capsMockLLM{}
	wrapped := CapsMiddleware(inner, DisabledCaps(CapTemperature))

	temp := 0.8
	_, _, _ = wrapped.Generate(context.Background(), nil, WithTemperature(temp))
	if inner.lastOpts.Temperature != nil {
		t.Fatalf("expected temperature=nil after caps filter, got %v", *inner.lastOpts.Temperature)
	}
}

func TestCapsMiddleware_DisableJSONSchema_Downgrade(t *testing.T) {
	inner := &capsMockLLM{}
	wrapped := CapsMiddleware(inner, DisabledCaps(CapJSONSchema))

	schema := JSONSchemaParam{Name: "test", Schema: map[string]any{"type": "object"}}
	_, _, _ = wrapped.Generate(context.Background(), nil, WithJSONSchema(schema))

	if inner.lastOpts.JSONSchema != nil {
		t.Fatal("expected JSONSchema=nil after disabling CapJSONSchema")
	}
	if inner.lastOpts.JSONMode == nil || !*inner.lastOpts.JSONMode {
		t.Fatal("expected JSONMode=true after CapJSONSchema downgrade")
	}
}

func TestCapsMiddleware_DisableJSONSchema_NoopWithoutSchema(t *testing.T) {
	inner := &capsMockLLM{}
	wrapped := CapsMiddleware(inner, DisabledCaps(CapJSONSchema))

	_, _, _ = wrapped.Generate(context.Background(), nil, WithTemperature(0.5))

	if inner.lastOpts.JSONMode != nil {
		t.Fatal("downgradeJSONSchema should not set JSONMode when JSONSchema was not set")
	}
}

func TestCapsMiddleware_DisableJSONMode(t *testing.T) {
	inner := &capsMockLLM{}
	wrapped := CapsMiddleware(inner, DisabledCaps(CapJSONMode))

	_, _, _ = wrapped.Generate(context.Background(), nil, WithJSONMode(true))

	if inner.lastOpts.JSONMode != nil {
		t.Fatal("expected JSONMode=nil after disabling CapJSONMode")
	}
}

func TestCapsMiddleware_AllCaps(t *testing.T) {
	inner := &capsMockLLM{}
	wrapped := CapsMiddleware(inner, DisabledCaps(CapTemperature, CapJSONSchema, CapJSONMode))

	schema := JSONSchemaParam{Name: "test", Schema: map[string]any{"type": "object"}}
	_, _, _ = wrapped.Generate(context.Background(), nil,
		WithTemperature(0.8),
		WithJSONSchema(schema),
		WithJSONMode(true),
	)

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

func TestCapsMiddleware_Stream(t *testing.T) {
	inner := &capsMockLLM{}
	wrapped := CapsMiddleware(inner, DisabledCaps(CapTemperature))

	_, _ = wrapped.GenerateStream(context.Background(), nil, WithTemperature(0.8))
	if inner.lastOpts.Temperature != nil {
		t.Fatal("expected temperature cleared in stream path")
	}
}

func TestMergeCaps(t *testing.T) {
	a := DisabledCaps(CapTemperature)
	b := DisabledCaps(CapJSONSchema)
	merged := mergeCaps(a, b)

	if merged.Supports(CapTemperature) || merged.Supports(CapJSONSchema) || !merged.Supports(CapJSONMode) {
		t.Fatalf("mergeCaps unexpected result: %+v", merged)
	}
}

func TestModelCaps_Supports(t *testing.T) {
	caps := DisabledCaps(CapTemperature)
	if caps.Supports(CapTemperature) {
		t.Fatal("expected CapTemperature disabled")
	}
	if !caps.Supports(CapJSONSchema) {
		t.Fatal("expected CapJSONSchema supported")
	}
}

func TestModelCaps_IsZero(t *testing.T) {
	if !(ModelCaps{}).IsZero() {
		t.Fatal("zero-value should be IsZero")
	}
	if DisabledCaps(CapTemperature).IsZero() {
		t.Fatal("non-empty should not be IsZero")
	}
}
