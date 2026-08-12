package openai

// Framework conformance: this file wires the provider into the shared
// inferencetest suites. Generate suites run against a captured HTTP server;
// compiler suites run without transport. The provider has no realtime
// session surface (deferred), so the session suites do not apply here.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
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
		compileGenerate("gpt-5.6-sol", catalog["gpt-5.6-sol"]),
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
	server, _ := newCapturedOpenAI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, responsesResponseJSON([]map[string]any{
			textOutputItem("ok"),
		}))
	})
	defer server.Close()
	calls := &inferencetest.Counter{}
	unary, _ := instrumentedGenerateDrivers(t, server, calls)

	inferencetest.RunGenerateUnary(t, inferencetest.GenerateUnarySuite{
		Model:   openaiModel("gpt-5.6-sol"),
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Driver:  unary,
		TransportCalls: func() int64 {
			return calls.Load()
		},
		AssertResponse: func(t *testing.T, response inference.GenerateResponse) {
			if len(response.Message.Content.Parts) != 1 {
				t.Fatalf("parts = %d", len(response.Message.Content.Parts))
			}
			text, ok := response.Message.Content.Parts[0].(message.TextPart)
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
	server, _ := newCapturedOpenAI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseBody(
			map[string]any{
				"type": "response.output_item.added", "output_index": 0,
				"item": map[string]any{"type": "message"},
			},
			map[string]any{
				"type": "response.output_text.delta", "output_index": 0, "delta": "ok",
			},
			map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id": "resp_1", "status": "completed",
					"usage": map[string]any{
						"input_tokens": 1, "output_tokens": 1, "total_tokens": 2,
						"input_tokens_details":  map[string]any{"cached_tokens": 0},
						"output_tokens_details": map[string]any{"reasoning_tokens": 0},
					},
				},
			},
		))
	})
	defer server.Close()
	calls := &inferencetest.Counter{}
	_, stream := instrumentedGenerateDrivers(t, server, calls)

	inferencetest.RunGenerateStream(t, inferencetest.GenerateStreamSuite{
		Model:   openaiModel("gpt-5.6-sol"),
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
			text, ok := response.Message.Content.Parts[0].(message.TextPart)
			if !ok || text.Text != "ok" {
				t.Fatalf("part = %#v", response.Message.Content.Parts[0])
			}
			if response.Usage.TotalTokens != 2 {
				t.Fatalf("usage = %+v", response.Usage)
			}
		},
	})
}

func TestConformanceGenerateCompileParity(t *testing.T) {
	server, _ := newCapturedOpenAI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		t.Error("parity checks are explain-only; transport must not run")
	})
	defer server.Close()
	calls := &inferencetest.Counter{}
	unary, stream := instrumentedGenerateDrivers(t, server, calls)

	inferencetest.RunGenerateCompileParity(t, inferencetest.GenerateCompileParitySuite{
		Model:   openaiModel("gpt-5.6-sol"),
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Unary:   unary,
		Stream:  stream,
	})
}

func TestConformanceGenerateCompiler(t *testing.T) {
	model := openaiModel("gpt-5.6-sol")

	inferencetest.RunGenerateCompiler(t, inferencetest.GenerateCompilerSuite[generateWire]{
		Model:   model,
		Shape:   inference.GenerateExecutionUnary,
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Snapshot: func(request inference.GenerateRequest) any {
			return request.Clone()
		},
		Compile: compileGenerate("gpt-5.6-sol", catalog["gpt-5.6-sol"]),
		AssertWire: func(t *testing.T, wire generateWire) {
			if wire.model != "gpt-5.6-sol" {
				t.Fatalf("wire model = %q", wire.model)
			}
			if wire.stream {
				t.Fatal("unary shape compiled a stream wire")
			}
		},
		Rejections: []inferencetest.CompilerRejection[inference.GenerateRequest]{
			{
				Name: "video intent has no surface",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Input.Content.Intent.Video = &inference.VideoIntent{}
					return request
				},
				Field: inference.FieldGenerateIntentVideo,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "reasoning is assistant-only",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Input.Content.Parts = append(
						request.Input.Content.Parts,
						message.ReasoningPart{
							Text:      "trace",
							Signature: "enc",
							ID:        "rs_1",
						},
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
					"extension.bytedance.generate_options.thinking",
				),
				Kind: inference.InvalidExtension,
			},
		},
		Drops: []inferencetest.CompilerDrop[inference.GenerateRequest]{
			{
				Name: "reasoning without encrypted payload cannot round-trip",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Context = append(request.Context, message.Message{
						Role: message.RoleAssistant,
						Content: message.Content{Parts: []message.Part{
							message.ReasoningPart{Text: "unsigned trace"},
							message.TextPart{Text: "answer"},
						}},
					})
					return request
				},
				Field: inference.FieldGenerateContextReasoning,
			},
		},
	})
}

func TestConformanceGenerateDataPartLowersToText(t *testing.T) {
	request := simpleTextRequest("hi")
	request.Input.Content.Parts = append(request.Input.Content.Parts, message.DataPart{
		MediaType: "application/vnd.example",
		Value:     json.RawMessage(`{"k":1}`),
	})
	compiled, err := compileGenerate("gpt-5.6-sol", catalog["gpt-5.6-sol"])(
		context.Background(), openaiModel("gpt-5.6-sol"), request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	var texts []string
	for _, item := range compiled.Wire.items {
		for _, content := range item.content {
			if content.kind == wireContentText {
				texts = append(texts, content.text)
			}
		}
	}
	if !strings.Contains(strings.Join(texts, ""), `{"k":1}`) {
		t.Fatalf("wire texts = %q", texts)
	}
}

// The capability matrix also needs a bare model: a custom declaration with
// no vision or reasoning support must reject those channels and still keep
// a complete ledger on plain text.
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
	image, err := media.NewImageURL("https://example.com/i.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}

	inferencetest.RunGenerateCompiler(t, inferencetest.GenerateCompilerSuite[generateWire]{
		Model:   openaiModel("my-plain-model"),
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
				Name: "image on model without image input",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Input.Content.Parts = append(
						request.Input.Content.Parts,
						message.ImagePart{Source: image},
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
					request.Input.Content.Intent.Text.ReasoningEffort = inference.ReasoningLow
					return request
				},
				Field: inference.FieldGenerateIntentReasoningEffort,
				Kind:  inference.UnsupportedFeature,
			},
		},
	})
}

func TestConformanceEmbedCompiler(t *testing.T) {
	embedRequest := func() inference.EmbedRequest {
		return inference.EmbedRequest{
			Items: []inference.EmbedItem{{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "hi"},
				}},
			}},
		}
	}

	inferencetest.RunCompiler(t, inferencetest.CompilerSuite[inference.EmbedRequest, embedWire]{
		Operation: inference.OperationEmbed,
		Model:     openaiModel("text-embedding-3-large"),
		Request:   embedRequest,
		Snapshot: func(request inference.EmbedRequest) any {
			return request.Clone()
		},
		Fields: func(request inference.EmbedRequest) []inference.FieldID {
			return request.ActiveFields()
		},
		Compile: compileEmbed("text-embedding-3-large", catalog["text-embedding-3-large"]),
		AssertWire: func(t *testing.T, wire embedWire) {
			if wire.model != "text-embedding-3-large" || len(wire.texts) != 1 {
				t.Fatalf("wire = %+v", wire)
			}
		},
		Rejections: []inferencetest.CompilerRejection[inference.EmbedRequest]{
			{
				Name: "multi-part item",
				Request: func() inference.EmbedRequest {
					return inference.EmbedRequest{
						Items: []inference.EmbedItem{{
							Content: message.Content{Parts: []message.Part{
								message.TextPart{Text: "a"},
								message.TextPart{Text: "b"},
							}},
						}},
					}
				},
				Field: inference.FieldEmbedItemMultiPart,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "image part",
				Request: func() inference.EmbedRequest {
					image, err := media.NewImageURL("https://example.com/i.png", "image/png")
					if err != nil {
						t.Fatal(err)
					}
					return inference.EmbedRequest{
						Items: []inference.EmbedItem{{
							Content: message.Content{Parts: []message.Part{
								message.ImagePart{Source: image},
							}},
						}},
					}
				},
				Field: inference.FieldEmbedItemImage,
				Kind:  inference.UnsupportedFeature,
			},
		},
	})

	// Dimensions must reject on a fixed-size model: ada-002 has no
	// dimensions parameter.
	inferencetest.RunCompiler(t, inferencetest.CompilerSuite[inference.EmbedRequest, embedWire]{
		Operation: inference.OperationEmbed,
		Model:     openaiModel("text-embedding-ada-002"),
		Request:   embedRequest,
		Snapshot: func(request inference.EmbedRequest) any {
			return request.Clone()
		},
		Fields: func(request inference.EmbedRequest) []inference.FieldID {
			return request.ActiveFields()
		},
		Compile: compileEmbed("text-embedding-ada-002", catalog["text-embedding-ada-002"]),
		AssertWire: func(t *testing.T, wire embedWire) {
			if wire.model != "text-embedding-ada-002" {
				t.Fatalf("wire = %+v", wire)
			}
		},
		Rejections: []inferencetest.CompilerRejection[inference.EmbedRequest]{
			{
				Name: "dimensions on fixed-size model",
				Request: func() inference.EmbedRequest {
					return inference.EmbedRequest{
						Items: []inference.EmbedItem{{
							Content: message.Content{Parts: []message.Part{
								message.TextPart{Text: "hi"},
							}},
						}},
						Dimensions: intPointer(512),
					}
				},
				Field: inference.FieldEmbedDimensions,
				Kind:  inference.UnsupportedFeature,
			},
		},
	})

	// Dimensions compile through on the text-embedding-3 family.
	compiled, err := compileEmbed("text-embedding-3-large", catalog["text-embedding-3-large"])(
		context.Background(),
		openaiModel("text-embedding-3-large"),
		inference.EmbedRequest{
			Items: []inference.EmbedItem{{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "hi"},
				}},
			}},
			Dimensions: intPointer(512),
		},
	)
	if err != nil {
		t.Fatalf("dimensions request rejected: %v", err)
	}
	if compiled.Wire.dimensions == nil || *compiled.Wire.dimensions != 512 {
		t.Fatalf("wire dimensions = %v", compiled.Wire.dimensions)
	}
}

func TestConformanceEmbedDataPartLowersToText(t *testing.T) {
	request := inference.EmbedRequest{Items: []inference.EmbedItem{{
		Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: "hi"},
			message.DataPart{
				MediaType: "application/vnd.example",
				Value:     json.RawMessage(`{"k":1}`),
			},
		}},
	}}}
	compiled, err := compileEmbed("text-embedding-3-large", catalog["text-embedding-3-large"])(
		context.Background(), openaiModel("text-embedding-3-large"), request,
	)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(compiled.Wire.texts) != 1 ||
		!strings.Contains(compiled.Wire.texts[0], `{"k":1}`) {
		t.Fatalf("wire texts = %+v", compiled.Wire.texts)
	}
}

func TestConformanceImageCompiler(t *testing.T) {
	imageRequest := func() inference.GenerateRequest {
		return inference.GenerateRequest{
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: "draw a rocket"},
					}},
					Intent: inference.Intent{Image: &inference.ImageIntent{}},
				},
			},
		}
	}

	inferencetest.RunGenerateCompiler(t, inferencetest.GenerateCompilerSuite[imageWire]{
		Model:   openaiModel("gpt-image-2"),
		Shape:   inference.GenerateExecutionUnary,
		Request: imageRequest,
		Snapshot: func(request inference.GenerateRequest) any {
			return request.Clone()
		},
		Compile: compileImage("gpt-image-2"),
		AssertWire: func(t *testing.T, wire imageWire) {
			if wire.model != "gpt-image-2" || wire.prompt != "draw a rocket" {
				t.Fatalf("wire = %+v", wire)
			}
		},
		Rejections: []inferencetest.CompilerRejection[inference.GenerateRequest]{
			{
				Name: "seed has no parameter",
				Request: func() inference.GenerateRequest {
					request := imageRequest()
					seed := int64(7)
					request.Input.Content.Intent.Image.Seed = &seed
					return request
				},
				Field: inference.FieldGenerateIntentImageSeed,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "size outside the enum",
				Request: func() inference.GenerateRequest {
					request := imageRequest()
					request.Input.Content.Intent.Image.Size = &media.ImageSize{
						Width: 800, Height: 600,
					}
					return request
				},
				Field: inference.FieldGenerateIntentImageSize,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "URL delivery",
				Request: func() inference.GenerateRequest {
					request := imageRequest()
					request.Input.Content.Intent.Image.Delivery = media.SourceURL
					return request
				},
				Field: inference.FieldGenerateIntentImageDelivery,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "text intent alongside image",
				Request: func() inference.GenerateRequest {
					request := imageRequest()
					request.Input.Content.Intent.Text = &inference.TextIntent{}
					return request
				},
				Field: inference.FieldGenerateIntentText,
				Kind:  inference.UnsupportedFeature,
			},
		},
	})
}

func TestConformanceTTSCompiler(t *testing.T) {
	ttsRequest := func() inference.GenerateRequest {
		return inference.GenerateRequest{
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: "hello world"},
					}},
					Intent: inference.Intent{Audio: &inference.AudioIntent{
						Voice:  media.VoiceSpec{ID: "alloy"},
						Format: media.AudioFormat{Encoding: media.AudioEncodingMP3},
					}},
				},
			},
		}
	}

	inferencetest.RunGenerateCompiler(t, inferencetest.GenerateCompilerSuite[ttsWire]{
		Model:   openaiModel("gpt-4o-mini-tts"),
		Shape:   inference.GenerateExecutionUnary,
		Request: ttsRequest,
		Snapshot: func(request inference.GenerateRequest) any {
			return request.Clone()
		},
		Compile: compileTTS("gpt-4o-mini-tts"),
		AssertWire: func(t *testing.T, wire ttsWire) {
			if wire.model != "gpt-4o-mini-tts" || wire.text != "hello world" {
				t.Fatalf("wire = %+v", wire)
			}
			if wire.voice != "alloy" || wire.format != "mp3" {
				t.Fatalf("wire voice = %q format = %q", wire.voice, wire.format)
			}
		},
		Rejections: []inferencetest.CompilerRejection[inference.GenerateRequest]{
			{
				Name: "speed outside range",
				Request: func() inference.GenerateRequest {
					request := ttsRequest()
					speed := 8.0
					request.Input.Content.Intent.Audio.Speed = &speed
					return request
				},
				Field: inference.FieldGenerateIntentAudioSpeed,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "sample rate has no parameter",
				Request: func() inference.GenerateRequest {
					request := ttsRequest()
					request.Input.Content.Intent.Audio.Format.SampleRateHz = 16000
					return request
				},
				Field: inference.FieldGenerateIntentAudioFormatSampleRate,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "language is fixed to the voice",
				Request: func() inference.GenerateRequest {
					request := ttsRequest()
					request.Input.Content.Intent.Audio.Voice.Language = "zh"
					return request
				},
				Field: inference.FieldGenerateIntentAudioVoiceLanguage,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "text intent alongside audio",
				Request: func() inference.GenerateRequest {
					request := ttsRequest()
					request.Input.Content.Intent.Text = &inference.TextIntent{}
					return request
				},
				Field: inference.FieldGenerateIntentText,
				Kind:  inference.UnsupportedFeature,
			},
		},
	})
}
