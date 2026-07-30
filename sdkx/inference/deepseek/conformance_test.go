package deepseek

// Framework conformance: this file wires the provider into the shared
// inferencetest suites against a fake chat completions server. Event
// mapping itself is covered by the driver-level tests in
// generate_more_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/inferencetest"
)

// countingTransport wraps one pipeline transport stage with a probe.
func countingTransport[Wire, Raw any](
	calls *inferencetest.Counter,
	next inference.Transport[Wire, Raw],
) inference.Transport[Wire, Raw] {
	return func(ctx context.Context, wire Wire) (Raw, error) {
		calls.Inc()
		return next(ctx, wire)
	}
}

// instrumentedGenerateDrivers binds the generate pipeline directly (no
// factory) so the transport probe sits inside the bound drivers.
func instrumentedGenerateDrivers(
	t *testing.T,
	server *chatServer,
	calls *inferencetest.Counter,
) (inference.GenerateDriver, inference.GenerateStreamDriver) {
	t.Helper()
	cls := server.clients(t)
	operations, err := inference.BindGenerateOperations(
		compileGenerate("deepseek-v4-pro", catalog["deepseek-v4-pro"]),
		countingTransport(calls, transportGenerate(cls.api)),
		decodeGenerate,
		countingTransport(calls, transportGenerateStream(cls.api)),
		decodeGenerateStream,
	)
	if err != nil {
		t.Fatalf("BindGenerateOperations: %v", err)
	}
	return operations.Unary, operations.Stream
}

func TestConformanceGenerateUnary(t *testing.T) {
	server := newChatServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, chatCompletionJSON("stop", nil))
	})
	calls := &inferencetest.Counter{}
	unary, _ := instrumentedGenerateDrivers(t, server, calls)

	inferencetest.RunGenerateUnary(t, inferencetest.GenerateUnarySuite{
		Model:   generateModel("deepseek-v4-pro"),
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Driver:  unary,
		TransportCalls: func() int64 {
			return calls.Load()
		},
		AssertResponse: func(t *testing.T, response inference.GenerateResponse) {
			if len(response.Message.Content.Parts) != 1 {
				t.Fatalf("parts = %d", len(response.Message.Content.Parts))
			}
			text, ok := response.Message.Content.Parts[0].(inference.TextPart)
			if !ok || text.Text != "ok" {
				t.Fatalf("part = %#v", response.Message.Content.Parts[0])
			}
			if response.FinishReason != inference.FinishCompleted {
				t.Fatalf("finish = %q", response.FinishReason)
			}
		},
	})
}

func TestConformanceGenerateStream(t *testing.T) {
	server := newChatServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseBody(textChunk("ok"), finishChunk("stop"), usageChunk()))
	})
	calls := &inferencetest.Counter{}
	_, stream := instrumentedGenerateDrivers(t, server, calls)

	inferencetest.RunGenerateStream(t, inferencetest.GenerateStreamSuite{
		Model:   generateModel("deepseek-v4-pro"),
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Driver:  stream,
		TransportCalls: func() int64 {
			return calls.Load()
		},
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
			if response.Usage.TotalTokens != 19 {
				t.Fatalf("usage = %+v", response.Usage)
			}
		},
	})
}

func TestConformanceGenerateCompileParity(t *testing.T) {
	server := newChatServer(t, func(w http.ResponseWriter, _ map[string]any) {
		t.Error("parity checks are explain-only; transport must not run")
	})
	calls := &inferencetest.Counter{}
	unary, stream := instrumentedGenerateDrivers(t, server, calls)

	inferencetest.RunGenerateCompileParity(t, inferencetest.GenerateCompileParitySuite{
		Model:   generateModel("deepseek-v4-pro"),
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Unary:   unary,
		Stream:  stream,
	})
}

func TestConformanceGenerateCompiler(t *testing.T) {
	inferencetest.RunGenerateCompiler(t, inferencetest.GenerateCompilerSuite[generateWire]{
		Model:   generateModel("deepseek-v4-pro"),
		Shape:   inference.GenerateExecutionUnary,
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Snapshot: func(request inference.GenerateRequest) any {
			return request.Clone()
		},
		Compile: compileGenerate("deepseek-v4-pro", catalog["deepseek-v4-pro"]),
		AssertWire: func(t *testing.T, wire generateWire) {
			if wire.model != "deepseek-v4-pro" {
				t.Fatalf("wire model = %q", wire.model)
			}
			if wire.stream {
				t.Fatal("unary shape compiled a stream wire")
			}
		},
		Rejections: []inferencetest.CompilerRejection[inference.GenerateRequest]{
			{
				Name: "data part has no representation",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Input.Content.Parts = append(
						request.Input.Content.Parts,
						inference.DataPart{
							MediaType: "application/vnd.example",
							Value:     json.RawMessage(`{"k":1}`),
						},
					)
					return request
				},
				Field: inference.FieldGenerateInputData,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "reasoning is assistant-only",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Input.Content.Parts = append(
						request.Input.Content.Parts,
						inference.ReasoningPart{Text: "trace"},
					)
					return request
				},
				Field: inference.FieldGenerateInputReasoning,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "image intent on text model",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Input.Content.Intent.Image = &inference.ImageIntent{}
					return request
				},
				Field: inference.FieldGenerateIntentImage,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "schema-constrained output unsupported",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Input.Content.Intent.Text.Response = &inference.ResponseFormat{
						Kind:   inference.ResponseJSONSchema,
						Name:   "answer",
						Schema: json.RawMessage(`{"type":"object"}`),
					}
					return request
				},
				Field: inference.FieldGenerateIntentTextResponseKind,
				Kind:  inference.UnsupportedFeature,
			},
	},
		Drops: []inferencetest.CompilerDrop[inference.GenerateRequest]{
			{
				Name: "reasoning without tool calls has no channel",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Context = append(request.Context, inference.Message{
						Role: inference.RoleAssistant,
						Content: inference.Content{Parts: []inference.Part{
							inference.ReasoningPart{Text: "trace"},
							inference.TextPart{Text: "answer"},
						}},
					})
					return request
				},
				Field: inference.FieldGenerateContextReasoning,
			},
		},
	})
}

// The capability matrix also needs a bare model: a custom declaration
// without reasoning support must reject the thinking channels and still
// keep a complete ledger on plain text.
func TestConformanceGenerateCompilerPlainModel(t *testing.T) {
	spec, err := decodeSpec([]byte(
		`{"models":[{"name":"my-plain-model","kind":"generate"}]}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}

	inferencetest.RunGenerateCompiler(t, inferencetest.GenerateCompilerSuite[generateWire]{
		Model:   generateModel("my-plain-model"),
		Shape:   inference.GenerateExecutionUnary,
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Snapshot: func(request inference.GenerateRequest) any {
			return request.Clone()
		},
		Compile: compileGenerate("my-plain-model", models["my-plain-model"]),
		AssertWire: func(t *testing.T, wire generateWire) {
			if wire.model != "my-plain-model" {
				t.Fatalf("wire model = %q", wire.model)
			}
		},
		Rejections: []inferencetest.CompilerRejection[inference.GenerateRequest]{
			{
			Name: "reasoning on non-thinking model",
			Request: func() inference.GenerateRequest {
				request := simpleTextRequest("hi")
				request.Input.Content.Intent.Text.ReasoningEffort = inference.ReasoningLow
				return request
			},
			Field: inference.FieldGenerateIntentReasoningEffort,
				Kind:  inference.UnsupportedFeature,
			},
		},
	})
}
