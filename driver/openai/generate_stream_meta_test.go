package openai

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

var _ inference.ProviderStreamMetadata = (*chatStream)(nil)
var _ inference.ProviderStreamMetadata = (*responsesStream)(nil)
var _ inference.ProviderStreamMetadata = (*imageStream)(nil)

func TestResponsesStreamTransportCapturesRequestID(t *testing.T) {
	server, _ := newCapturedOpenAI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		if r.URL.Path != "/responses" {
			t.Errorf("path = %q, want /responses", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-request-id", "req_openai_responses_1")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	})
	defer server.Close()

	entry := catalogEntry{
		kind:         kindGenerate,
		api:          apiResponses,
		capabilities: generateChatCapabilities().WithReasoning(inference.ReasoningToggle),
	}
	compiled, err := compileGenerate("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		simpleTextRequest("hi"),
		inference.GenerateExecutionStream,
	)
	if err != nil {
		t.Fatalf("compile responses stream: %v", err)
	}
	stream, err := transportGenerateStream(testClients(t, server).api)(
		context.Background(),
		compiled.Wire,
	)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	meta, ok := stream.(inference.ProviderStreamMetadata)
	if !ok {
		t.Fatal("responses stream must expose provider stream metadata")
	}
	if got := meta.RequestID(); got != "req_openai_responses_1" {
		t.Fatalf("stream request id = %q, want req_openai_responses_1", got)
	}
}

func TestChatStreamEndReportsSynthesizedFinishOnlyWithoutExplicitReason(t *testing.T) {
	truncated := &chatStream{sawTools: true}
	if got := truncated.end(); got != string(inference.FinishToolCalls) {
		t.Fatalf("end() = %q, want synthesized tool_calls", got)
	}
	if len(truncated.pending) != 1 ||
		!truncated.pending[0].synthesized ||
		truncated.pending[0].finish != inference.FinishToolCalls {
		t.Fatalf("synthesized finish payload = %+v, want synthesized tool_calls",
			truncated.pending)
	}
	if got := truncated.end(); got != "" {
		t.Fatalf("second end() = %q, want empty", got)
	}

	explicit := &chatStream{finish: inference.FinishCompleted}
	if got := explicit.end(); got != "" {
		t.Fatalf("end() with explicit finish = %q, want empty", got)
	}
	if len(explicit.pending) != 1 ||
		explicit.pending[0].synthesized ||
		explicit.pending[0].finish != inference.FinishCompleted {
		t.Fatalf("explicit finish payload = %+v, want non-synthesized completed",
			explicit.pending)
	}

	textOnly := &chatStream{}
	if got := textOnly.end(); got != string(inference.FinishCompleted) {
		t.Fatalf("end() without tools/finish = %q, want synthesized completed", got)
	}
	if len(textOnly.pending) != 1 ||
		!textOnly.pending[0].synthesized {
		t.Fatalf("text-only synthesized payload = %+v, want synthesized", textOnly.pending)
	}
}
