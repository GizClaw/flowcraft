package minimax

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/inferencetest"
	"github.com/GizClaw/flowcraft/sdk/message"
)

// The drivers bind through the anthropic kernel, whose compiler, wire
// model, and decoders already run the full conformance suites — including
// the compiler suite — in the anthropic package. These suites verify the
// end-to-end pipeline against the kernel-supplied drivers bound with
// MiniMax capabilities; the transport probe counts HTTP requests at the
// fake server because the kernel owns the transport stage.

func TestConformanceGenerateUnary(t *testing.T) {
	server := newMessagesServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, messageJSON([]map[string]any{
			{"type": "text", "text": "ok"},
		}))
	})

	ops, err := openGenerate(server.clients(t), catalog["MiniMax-M3"],
		inference.ModelID{Provider: "minimax", Name: "MiniMax-M3"}, "default")
	if err != nil {
		t.Fatalf("openGenerate: %v", err)
	}

	inferencetest.RunGenerateUnary(t, inferencetest.GenerateUnarySuite{
		Model:          minimaxModel("MiniMax-M3"),
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
			text, ok := response.Message.Content.Parts[0].(message.TextPart)
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
	server := newMessagesServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, textStreamBody("ok"))
	})

	ops, err := openGenerate(server.clients(t), catalog["MiniMax-M3"],
		inference.ModelID{Provider: "minimax", Name: "MiniMax-M3"}, "default")
	if err != nil {
		t.Fatalf("openGenerate: %v", err)
	}

	inferencetest.RunGenerateStream(t, inferencetest.GenerateStreamSuite{
		Model:          minimaxModel("MiniMax-M3"),
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
			text, ok := response.Message.Content.Parts[0].(message.TextPart)
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
	server := newMessagesServer(t, func(w http.ResponseWriter, _ map[string]any) {
		t.Error("parity checks are explain-only; transport must not run")
	})

	ops, err := openGenerate(server.clients(t), catalog["MiniMax-M3"],
		inference.ModelID{Provider: "minimax", Name: "MiniMax-M3"}, "default")
	if err != nil {
		t.Fatalf("openGenerate: %v", err)
	}

	inferencetest.RunGenerateCompileParity(t, inferencetest.GenerateCompileParitySuite{
		Model:   minimaxModel("MiniMax-M3"),
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Unary:   ops.Unary,
		Stream:  ops.Stream,
	})
}
