package anthropic

// Framework conformance: this file wires the provider into the shared
// inferencetest suites against a captured HTTP server. Claude serves the
// Messages API only, so the suites cover generate unary, stream, parity,
// and the compiler ledger.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/inferencetest"
	"github.com/GizClaw/flowcraft/sdk/inference/media"
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
	server *httptest.Server,
	calls *inferencetest.Counter,
) (inference.GenerateDriver, inference.GenerateStreamDriver) {
	t.Helper()
	cls := testClients(t, server)
	operations, err := inference.BindGenerateOperations(
		compileGenerate("claude-sonnet-5", catalog["claude-sonnet-5"]),
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
	server, _ := newCapturedAnthropic(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, messageJSON([]map[string]any{
			{"type": "text", "text": "ok"},
		}))
	})
	defer server.Close()
	calls := &inferencetest.Counter{}
	unary, _ := instrumentedGenerateDrivers(t, server, calls)

	inferencetest.RunGenerateUnary(t, inferencetest.GenerateUnarySuite{
		Model:   claudeModel("claude-sonnet-5"),
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
			if response.Usage.Input.CacheReadTokens == nil ||
				*response.Usage.Input.CacheReadTokens != 3 {
				t.Fatalf("cache read = %+v", response.Usage.Input)
			}
		},
	})
}

func TestConformanceGenerateStream(t *testing.T) {
	server, _ := newCapturedAnthropic(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseBody(
			map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id": "msg_1", "type": "message", "role": "assistant",
					"model": "claude-sonnet-5", "content": []any{},
					"usage": map[string]any{
						"input_tokens": 1, "output_tokens": 0,
						"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
					},
				},
			},
			map[string]any{
				"type": "content_block_start", "index": 0,
				"content_block": map[string]any{"type": "text", "text": ""},
			},
			map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": "ok"},
			},
			map[string]any{"type": "content_block_stop", "index": 0},
			map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": "end_turn"},
				"usage": map[string]any{"output_tokens": 1},
			},
			map[string]any{"type": "message_stop"},
		))
	})
	defer server.Close()
	calls := &inferencetest.Counter{}
	_, stream := instrumentedGenerateDrivers(t, server, calls)

	inferencetest.RunGenerateStream(t, inferencetest.GenerateStreamSuite{
		Model:   claudeModel("claude-sonnet-5"),
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
			if response.Usage.OutputTokens != 1 || response.Usage.InputTokens != 1 {
				t.Fatalf("usage = %+v", response.Usage)
			}
		},
	})
}

func TestConformanceGenerateCompileParity(t *testing.T) {
	server, _ := newCapturedAnthropic(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		t.Error("parity checks are explain-only; transport must not run")
	})
	defer server.Close()
	calls := &inferencetest.Counter{}
	unary, stream := instrumentedGenerateDrivers(t, server, calls)

	inferencetest.RunGenerateCompileParity(t, inferencetest.GenerateCompileParitySuite{
		Model:   claudeModel("claude-sonnet-5"),
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Unary:   unary,
		Stream:  stream,
	})
}

func TestConformanceGenerateCompiler(t *testing.T) {
	model := claudeModel("claude-sonnet-5")

	inferencetest.RunGenerateCompiler(t, inferencetest.GenerateCompilerSuite[generateWire]{
		Model:   model,
		Shape:   inference.GenerateExecutionUnary,
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Snapshot: func(request inference.GenerateRequest) any {
			return request.Clone()
		},
		Compile: compileGenerate("claude-sonnet-5", catalog["claude-sonnet-5"]),
		AssertWire: func(t *testing.T, wire generateWire) {
			if wire.model != "claude-sonnet-5" {
				t.Fatalf("wire model = %q", wire.model)
			}
			if wire.stream {
				t.Fatal("unary shape compiled a stream wire")
			}
			if wire.maxTokens != DefaultMaxTokens {
				t.Fatalf("wire maxTokens = %d", wire.maxTokens)
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
				Name: "audio part has no surface",
				Request: func() inference.GenerateRequest {
					clip, err := media.NewAudioBytes([]byte{1, 2}, "audio/wav")
					if err != nil {
						t.Fatal(err)
					}
					request := simpleTextRequest("hi")
					request.Input.Content.Parts = append(
						request.Input.Content.Parts,
						inference.AudioPart{Source: clip},
					)
					return request
				},
				Field: inference.FieldGenerateInputAudio,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "json_object mode does not exist",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Input.Content.Intent.Text = &inference.TextIntent{
						Response: &inference.ResponseFormat{
							Kind: inference.ResponseJSONObject,
						},
					}
					return request
				},
				Field: inference.FieldGenerateIntentTextResponseKind,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "image intent has no surface",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Input.Content.Intent.Image = &inference.ImageIntent{}
					return request
				},
				Field: inference.FieldGenerateIntentImage,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "reasoning is assistant-only",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Input.Content.Parts = append(
						request.Input.Content.Parts,
						inference.ReasoningPart{Text: "trace", Signature: "sig"},
					)
					return request
				},
				Field: inference.FieldGenerateInputReasoning,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "foreign extension",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Extensions = inference.Extensions{foreignExtension{}}
					return request
				},
				Field: inference.FieldID(
					"extension.openai.generate_options.thinking",
				),
				Kind: inference.InvalidExtension,
			},
		},
		Drops: []inferencetest.CompilerDrop[inference.GenerateRequest]{
			{
				Name: "unsigned reasoning cannot round-trip",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Context = append(request.Context, inference.Message{
						Role: inference.RoleAssistant,
						Content: inference.Content{Parts: []inference.Part{
							inference.ReasoningPart{Text: "unsigned trace"},
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

// A custom bare declaration must reject vision and reasoning channels.
func TestConformanceGenerateCompilerPlainModel(t *testing.T) {
	spec, err := decodeSpec([]byte(
		`{"models":[{"name":"my-claude"}]}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	image, err := media.NewImageURL("https://example.com/i.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}

	inferencetest.RunGenerateCompiler(t, inferencetest.GenerateCompilerSuite[generateWire]{
		Model:   claudeModel("my-claude"),
		Shape:   inference.GenerateExecutionUnary,
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Snapshot: func(request inference.GenerateRequest) any {
			return request.Clone()
		},
		Compile: compileGenerate("my-claude", models["my-claude"]),
		AssertWire: func(t *testing.T, wire generateWire) {
			if wire.model != "my-claude" {
				t.Fatalf("wire model = %q", wire.model)
			}
		},
		Rejections: []inferencetest.CompilerRejection[inference.GenerateRequest]{
			{
				Name: "image on model without image input",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Input.Content.Parts = append(
						request.Input.Content.Parts,
						inference.ImagePart{Source: image},
					)
					return request
				},
				Field: inference.FieldGenerateInputImage,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "reasoning on non-reasoning model",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Input.Content.Intent.Reasoning =
						&inference.ReasoningIntent{Effort: inference.ReasoningLow}
					return request
				},
				Field: inference.FieldGenerateIntentReasoningEffort,
				Kind:  inference.UnsupportedFeature,
			},
		},
	})
}
