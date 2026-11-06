package inference

import (
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel/attribute"
)

func TestProviderLogAttrs(t *testing.T) {
	attrs := ProviderLogAttrs("openai", "generate", "gpt-4o", nil, "req-1", "resp-1")
	got := map[string]string{}
	for _, kv := range attrs {
		got[string(kv.Key)] = kv.Value.AsString()
	}
	if got[telemetry.AttrGenAIProviderName] != "openai" {
		t.Errorf("provider = %q", got[telemetry.AttrGenAIProviderName])
	}
	if got[telemetry.AttrGenAIRequestModel] != "gpt-4o" {
		t.Errorf("model = %q", got[telemetry.AttrGenAIRequestModel])
	}
	if got[telemetry.AttrLLMRequestID] != "req-1" {
		t.Errorf("request id = %q", got[telemetry.AttrLLMRequestID])
	}
	if got[telemetry.AttrGenAIResponseID] != "resp-1" {
		t.Errorf("response id = %q", got[telemetry.AttrGenAIResponseID])
	}
	if _, ok := got[telemetry.AttrErrorMessage]; ok {
		t.Error("a successful call carries error.message")
	}
}

// TestProviderLogAttrsClassifiesFailure pins that a classified error contributes
// its kind and the request id it carries, without the driver having to pass them.
func TestProviderLogAttrsClassifiesFailure(t *testing.T) {
	failure := NewError(ProviderFailure, "", errors.New("upstream down")).WithDetail("transport.do")
	failure.RequestID = "req-from-error"

	attrs := ProviderLogAttrs("openai", "generate", "", failure, "", "")
	got := map[string]attribute.Value{}
	for _, kv := range attrs {
		got[string(kv.Key)] = kv.Value
	}
	if got[telemetry.AttrErrorKind].AsString() != string(ProviderFailure) {
		t.Errorf("error.kind = %q", got[telemetry.AttrErrorKind].AsString())
	}
	if got[telemetry.AttrInferenceErrorDetail].AsString() != "transport.do" {
		t.Errorf("detail = %q", got[telemetry.AttrInferenceErrorDetail].AsString())
	}
	if got[telemetry.AttrLLMRequestID].AsString() != "req-from-error" {
		t.Errorf("request id = %q", got[telemetry.AttrLLMRequestID].AsString())
	}
	if got[telemetry.AttrGenAIRequestModel].AsString() != "" {
		t.Error("an empty model must be omitted")
	}
}
