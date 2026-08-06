package kimi

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/inferencetest"
	"github.com/GizClaw/flowcraft/sdk/message"
)

// The drivers bind this package's own compiler, transport, and decoders;
// these suites run the framework-level contract end to end against the
// fake endpoint, with the transport probe counting HTTP requests at the
// server.

func TestConformanceGenerateUnary(t *testing.T) {
	server := newKimiServer(t, func(w http.ResponseWriter, _ map[string]any) {
		fmt.Fprint(w, textCompletion("ok"))
	})

	ops := server.bindOps(t, "moonshot-v1-8k")

	inferencetest.RunGenerateUnary(t, inferencetest.GenerateUnarySuite{
		Model:          kimiModel("moonshot-v1-8k"),
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
			if response.Usage.InputTokens != 19 || response.Usage.OutputTokens != 21 {
				t.Fatalf("usage = %+v", response.Usage)
			}
		},
	})
}

func TestConformanceGenerateStream(t *testing.T) {
	server := newKimiServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, chunkBody(streamChunk(map[string]any{
			"role":    "assistant",
			"content": "ok",
		}, "stop", true)))
	})

	ops := server.bindOps(t, "moonshot-v1-8k")

	inferencetest.RunGenerateStream(t, inferencetest.GenerateStreamSuite{
		Model:          kimiModel("moonshot-v1-8k"),
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
			if response.Usage.InputTokens != 19 || response.Usage.OutputTokens != 13 {
				t.Fatalf("usage = %+v", response.Usage)
			}
		},
	})
}

func TestConformanceGenerateCompileParity(t *testing.T) {
	server := newKimiServer(t, func(w http.ResponseWriter, _ map[string]any) {
		t.Error("parity checks are explain-only; transport must not run")
	})

	ops := server.bindOps(t, "moonshot-v1-8k")

	inferencetest.RunGenerateCompileParity(t, inferencetest.GenerateCompileParitySuite{
		Model:   kimiModel("moonshot-v1-8k"),
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Unary:   ops.Unary,
		Stream:  ops.Stream,
	})
}

func TestConformanceCompilerRejections(t *testing.T) {
	inferencetest.RunCompiler(t, inferencetest.CompilerSuite[inference.GenerateRequest, generateWire]{
		Operation: inference.OperationGenerate,
		Model:     kimiModel("kimi-k3"),
		Request:   func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Snapshot: func(request inference.GenerateRequest) any {
			return request.Clone()
		},
		Fields: func(request inference.GenerateRequest) []inference.FieldID {
			return request.ActiveFieldsFor(inference.GenerateExecutionUnary)
		},
		Compile: func(ctx context.Context, model inference.ModelRef, request inference.GenerateRequest) (inference.Compiled[generateWire], error) {
			return compileGenerate(model.ID.Name, catalog[model.ID.Name])(ctx, model, request, inference.GenerateExecutionUnary)
		},
		AssertWire: func(t *testing.T, wire generateWire) {
			if wire.Model != "kimi-k3" || len(wire.Messages) != 1 {
				t.Fatalf("wire = %+v", wire)
			}
		},
	})
}
