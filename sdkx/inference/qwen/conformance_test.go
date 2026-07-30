package qwen

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/inferencetest"
)

// The drivers bind this package's own compiler, transport, and decoders;
// these suites run the framework-level contract end to end against the
// fake DashScope endpoint, with the transport probe counting HTTP
// requests at the server.

func TestConformanceGenerateUnary(t *testing.T) {
	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		fmt.Fprint(w, textEnvelope("ok"))
	})

	ops := server.bindOps(t, "qwen-plus")

	inferencetest.RunGenerateUnary(t, inferencetest.GenerateUnarySuite{
		Model:          qwenModel("qwen-plus"),
		Request:        func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Driver:         ops.Unary,
		TransportCalls: server.requests,
		AssertResponse: func(t *testing.T, response inference.GenerateResponse) {
			if response.FinishReason != inference.FinishCompleted {
				t.Fatalf("finish = %q", response.FinishReason)
			}
			if len(response.Message.Content.Parts) != 1 {
				t.Fatalf("parts = %d", len(response.Message.Content.Parts))
			}
			text, ok := response.Message.Content.Parts[0].(inference.TextPart)
			if !ok || text.Text != "ok" {
				t.Fatalf("part = %#v", response.Message.Content.Parts[0])
			}
			if response.Usage.InputTokens != 12 || response.Usage.OutputTokens != 7 {
				t.Fatalf("usage = %+v", response.Usage)
			}
		},
	})
}

func TestConformanceGenerateStream(t *testing.T) {
	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, dashSSEBody(streamChunk(map[string]any{
			"role":    "assistant",
			"content": "ok",
		}, "stop", true)))
	})

	ops := server.bindOps(t, "qwen-plus")

	inferencetest.RunGenerateStream(t, inferencetest.GenerateStreamSuite{
		Model:          qwenModel("qwen-plus"),
		Request:        func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Driver:         ops.Stream,
		TransportCalls: server.requests,
		AssertResult: func(t *testing.T, response inference.GenerateResponse) {
			if response.FinishReason != inference.FinishCompleted {
				t.Fatalf("finish = %q", response.FinishReason)
			}
			if len(response.Message.Content.Parts) != 1 {
				t.Fatalf("parts = %d", len(response.Message.Content.Parts))
			}
			text, ok := response.Message.Content.Parts[0].(inference.TextPart)
			if !ok || text.Text != "ok" {
				t.Fatalf("part = %#v", response.Message.Content.Parts[0])
			}
			if response.Usage.InputTokens != 12 || response.Usage.OutputTokens != 7 {
				t.Fatalf("usage = %+v", response.Usage)
			}
		},
	})
}

func TestConformanceGenerateCompileParity(t *testing.T) {
	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		t.Error("parity checks are explain-only; transport must not run")
	})

	ops := server.bindOps(t, "qwen-plus")

	inferencetest.RunGenerateCompileParity(t, inferencetest.GenerateCompileParitySuite{
		Model:   qwenModel("qwen-plus"),
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Unary:   ops.Unary,
		Stream:  ops.Stream,
	})
}
