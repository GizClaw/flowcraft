package openai

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// openaiChatParams names the chat wire type so the fixture reads without a
// package qualifier at every use.
type openaiChatParams = openai.ChatCompletionNewParams

// apiKey reads the credentialed lane's key.
func apiKey(t *testing.T) string {
	t.Helper()
	return os.Getenv("OPENAI_API_KEY")
}

// render wire values as JSON text, which is what the accounting suite compares
// markers against.
func renderJSON[T any](value T) (string, error) {
	raw, err := json.Marshal(value)
	return string(raw), err
}

// baseRequest is the smallest request this driver compiles.
func baseRequest(t *testing.T) inference.GenerateRequest {
	t.Helper()
	request := inference.GenerateRequest{
		Model: model.ModelRef{
			ID:      model.ModelID{Provider: ProviderID, Name: "gpt-5-mini"},
			Profile: "default",
		},
		Input: inference.GenerateInput{
			Role:    inference.InputRoleUser,
			Content: message.NewTextContent("hello"),
		},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("request: %v", err)
	}
	return request
}

// chatRequestWithTools is the request the decode samples expect: the model was
// shown one tool, so a call to it is defined and a call to anything else is not.
func chatRequestWithTools(t *testing.T) inference.GenerateRequest {
	t.Helper()
	request := baseRequest(t)
	tools, err := inference.NewToolset([]message.ToolDefinition{{
		Name:        "search",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}})
	if err != nil {
		t.Fatalf("toolset: %v", err)
	}
	request.Tools = tools
	if err := request.Validate(); err != nil {
		t.Fatalf("request: %v", err)
	}
	return request
}

func textSample(name, body string) inferencetest.Sample {
	return inferencetest.Sample{Name: name, Body: body}
}

func chatCompletionBody(text, finish string, toolCalls string) string {
	calls := ""
	if toolCalls != "" {
		calls = `,"tool_calls":[` + toolCalls + `]`
	}
	return `{"id":"chatcmpl-1","object":"chat.completion","created":0,"model":"gpt-5-mini",` +
		`"choices":[{"index":0,"finish_reason":"` + finish + `",` +
		`"message":{"role":"assistant","content":` + text + calls + `}}],` +
		`"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`
}

func TestChatConformance(t *testing.T) {
	d := &driver{settings: testSettings(APIChat)}
	newProvider := func(t *testing.T, baseURL string) (inference.Provider, error) {
		return New(Settings{API: APIChat, APIKey: "test-key", BaseURL: baseURL})
	}

	t.Run("compiler", func(t *testing.T) {
		tools := chatRequestWithTools(t)
		withChoice, err := tools.Tools.Choosing(inference.ToolChoice{Mode: inference.ToolChoiceAuto})
		if err != nil {
			t.Fatalf("choosing: %v", err)
		}
		withChoiceRequest := chatRequestWithTools(t)
		withChoiceRequest.Tools = withChoice

		withContext := baseRequest(t)
		withContext.Context = []message.Message{message.NewTextMessage(message.RoleUser, "earlier")}

		temperature := 0.3
		withOptions := baseRequest(t)
		withOptions.Extensions = inference.Extensions{&GenerationOptions{Temperature: &temperature}}

		toolPick := chatRequestWithTools(t)
		toolPick.Tools, _ = toolPick.Tools.Choosing(inference.ToolChoice{Mode: inference.ToolChoiceAuto})

		inferencetest.RunCompiler(t, inferencetest.CompilerFixture[openaiChatParams]{
			ProviderID: ProviderID,
			Compile: func(ctx context.Context, request inference.GenerateRequest, shape protocol.Shape) (protocol.Compiled[openaiChatParams], error) {
				return d.chatCompile(ctx, request, shape)
			},
			Baseline:  baseRequest(t),
			Supported: []inference.Field{inference.FieldContext, inference.FieldInput, inference.FieldRole, inference.FieldContent, inference.FieldOutput, inference.FieldTools, inference.FieldToolChoice, inference.FieldExtensions},
			Cases: []inferencetest.FieldCase{
				{Field: inference.FieldContext, Request: withContext, Marker: `"earlier"`},
				{Field: inference.FieldInput, Request: baseRequest(t), Marker: `"hello"`},
				{Field: inference.FieldRole, Request: baseRequest(t), Marker: `"role":"user"`},
				{Field: inference.FieldContent, Request: baseRequest(t), Marker: `"hello"`},
				// A text output adds nothing to the chat wire, so it is accounted
				// for without a marker.
				{Field: inference.FieldOutput, Request: withTextOutput(t)},
				{Field: inference.FieldTools, Request: tools, Marker: `"name":"search"`},
				{Field: inference.FieldToolChoice, Request: withChoiceRequest, Marker: `"tool_choice"`},
				{Field: inference.FieldExtensions, Request: withOptions, Marker: `"temperature":0.3`},
			},
			Render: renderJSON[openaiChatParams],
			Rejections: []inferencetest.Rejection{
				{Name: "image output", Request: imageRequest(t, "a square"), Kind: inference.UnsupportedFeature, Field: inference.FieldOutput},
				{Name: "responses-only option", Request: withMaxToolCalls(t), Kind: inference.UnsupportedFeature, Field: inference.FieldExtensions},
				{Name: "unknown extension", Request: withForeignExtension(t), Kind: inference.InvalidExtension, Field: inference.FieldExtensions},
			},
		})
	})

	t.Run("provider", func(t *testing.T) {
		inferencetest.RunProvider(t, inferencetest.ProviderFixture{
			ProviderID:  ProviderID,
			NewProvider: newProvider,
			Request:     chatRequestWithTools,
			Samples: map[inference.Output][]inferencetest.Sample{
				inference.OutputText: {
					{Name: "text", Body: chatCompletionBody(`"hello there"`, "stop", ""),
						Check: func(t *testing.T, response inference.GenerateResponse) {
							if got := response.Message.Content.Text(); got != "hello there" {
								t.Fatalf("text = %q", got)
							}
							if response.Usage.InputTokens != 5 || response.Usage.OutputTokens != 2 {
								t.Fatalf("usage = %+v", response.Usage)
							}
						}},
					{Name: "defined tool call", Body: chatCompletionBody("null", "tool_calls", `{"id":"c1","type":"function","function":{"name":"search","arguments":"{}"}}`),
						Check: func(t *testing.T, response inference.GenerateResponse) {
							calls := response.Message.ToolCalls()
							if len(calls) != 1 || calls[0].Name != "search" {
								t.Fatalf("tool calls = %+v", calls)
							}
							if response.FinishReason != inference.FinishToolCalls {
								t.Fatalf("finish = %q", response.FinishReason)
							}
						}},
					{Name: "undefined tool call", Body: chatCompletionBody("null", "tool_calls", `{"id":"c2","type":"function","function":{"name":"missing","arguments":"{}"}}`),
						WantKind: inference.UndefinedTool},
					{Name: "server error", Status: 503, Body: `{"error":{"message":"boom","type":"server_error"}}`,
						WantKind: inference.ProviderFailure},
					{Name: "unknown finish reason", Body: chatCompletionBody(`"x"`, "banana", ""),
						WantKind: inference.ProviderResponseUnusable},
				},
			},
		})
	})

	t.Run("stream", func(t *testing.T) {
		inferencetest.RunStream(t, inferencetest.StreamFixture{
			ProviderID:  ProviderID,
			NewProvider: newProvider,
			Request:     chatRequestWithTools,
			Samples: []inferencetest.StreamSample{
				{
					Name: "text with trailing usage",
					Body: chatStreamBody(),
					WantKinds: []inference.StreamEventKind{
						inference.StreamStart, inference.StreamText, inference.StreamText,
						inference.StreamEnd, inference.StreamUsage,
					},
					WantText:  "hello",
					WantUsage: &inferencetest.UsageExpectation{Input: 5, Output: 2, Total: 7},
					WantParts: []inferencetest.PartExpectation{{Kind: message.PartText, Text: "hello"}},
				},
				{
					Name: "tool call fragments",
					Body: chatToolStreamBody(),
					WantKinds: []inference.StreamEventKind{
						inference.StreamStart, inference.StreamToolCall,
						inference.StreamToolCall, inference.StreamToolCall, inference.StreamEnd,
					},
					WantCalls: []inferencetest.CallExpectation{
						{ID: "call_1", Name: "search", Arguments: `{"q":"go"}`},
					},
					WantParts: []inferencetest.PartExpectation{{Kind: message.PartToolCall}},
				},
			},
			Truncated: &inferencetest.StreamSample{Name: "no terminal event", Body: chatTruncatedStreamBody()},
			Equivalence: &inferencetest.Equivalence{
				Unary: inferencetest.ProviderFixture{
					ProviderID:  ProviderID,
					NewProvider: newProvider,
					Request:     chatRequestWithTools,
				},
				UnarySample: textSample("unary", chatCompletionBody(`"hello"`, "stop", "")),
				Stream:      inferencetest.StreamSample{Name: "equivalence", Body: chatStreamBody()},
				Compare: func(t *testing.T, unary, streamed inference.GenerateResponse) {
					if unary.Message.Content.Text() != streamed.Message.Content.Text() {
						t.Fatalf("text: unary %q vs streamed %q",
							unary.Message.Content.Text(), streamed.Message.Content.Text())
					}
					if unary.FinishReason != streamed.FinishReason {
						t.Fatalf("finish: %q vs %q", unary.FinishReason, streamed.FinishReason)
					}
					if unary.Usage.TotalTokens != streamed.Usage.TotalTokens {
						t.Fatalf("usage: %+v vs %+v", unary.Usage, streamed.Usage)
					}
				},
			},
		})
	})

	t.Run("credentialed", func(t *testing.T) {
		inferencetest.RunCredentialed(t, inferencetest.CredentialedFixture{
			EnvKey:      "OPENAI_API_KEY",
			NewProvider: func(t *testing.T) (inference.Provider, error) { return New(Settings{API: APIChat, APIKey: apiKey(t)}) },
			NewProviderWithKey: func(t *testing.T, key string) (inference.Provider, error) {
				return New(Settings{API: APIChat, APIKey: key})
			},
			Request: func(t *testing.T, _ inference.Output) inference.GenerateRequest {
				return chatRequestWithTools(t)
			},
			Outputs: []inference.Output{inference.OutputText},
		})
	})
}

// chatStreamBody is a complete chat SSE stream: two text deltas, a finish reason,
// then usage in a trailing chunk — the ordering the suite pins.
func chatStreamBody() string {
	return "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5-mini\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hel\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5-mini\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5-mini\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5-mini\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"
}

// chatTruncatedStreamBody stops after content, without a finish reason.
func chatTruncatedStreamBody() string {
	return "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5-mini\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hel\"}}]}\n\n"
}

// withTextOutput pins the output modality explicitly, which is what makes the
// output field active.
func withTextOutput(t *testing.T) inference.GenerateRequest {
	t.Helper()
	request := baseRequest(t)
	request.Output = inference.OutputText
	if err := request.Validate(); err != nil {
		t.Fatalf("request: %v", err)
	}
	return request
}

func withMaxToolCalls(t *testing.T) inference.GenerateRequest {
	t.Helper()
	limit := 2
	request := baseRequest(t)
	request.Extensions = inference.Extensions{&GenerationOptions{MaxToolCalls: &limit}}
	return request
}

func withForeignExtension(t *testing.T) inference.GenerateRequest {
	t.Helper()
	request := baseRequest(t)
	request.Extensions = inference.Extensions{foreignExtension{}}
	return request
}

func responsesBody(text, status string) string {
	return `{"id":"resp_1","object":"response","created_at":0,"model":"gpt-5-mini",` +
		`"status":"` + status + `","parallel_tool_calls":true,"tool_choice":"auto","tools":[],` +
		`"output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"` + text + `","annotations":[]}]}],` +
		`"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7,` +
		`"input_tokens_details":{"cached_tokens":0},` +
		`"output_tokens_details":{"reasoning_tokens":0}}}`
}

func TestResponsesConformance(t *testing.T) {
	d := &driver{settings: testSettings(APIResponses)}
	newProvider := func(t *testing.T, baseURL string) (inference.Provider, error) {
		return New(Settings{API: APIResponses, APIKey: "test-key", BaseURL: baseURL})
	}

	t.Run("compiler", func(t *testing.T) {
		tools := chatRequestWithTools(t)
		withChoice, err := tools.Tools.Choosing(inference.ToolChoice{Mode: inference.ToolChoiceAuto})
		if err != nil {
			t.Fatalf("choosing: %v", err)
		}
		withChoiceRequest := chatRequestWithTools(t)
		withChoiceRequest.Tools = withChoice

		withContext := baseRequest(t)
		withContext.Context = []message.Message{message.NewTextMessage(message.RoleUser, "earlier")}

		temperature := 0.3
		withOptions := baseRequest(t)
		withOptions.Extensions = inference.Extensions{&GenerationOptions{Temperature: &temperature}}

		inferencetest.RunCompiler(t, inferencetest.CompilerFixture[responses.ResponseNewParams]{
			ProviderID: ProviderID,
			Compile: func(ctx context.Context, request inference.GenerateRequest, shape protocol.Shape) (protocol.Compiled[responses.ResponseNewParams], error) {
				return d.responsesCompile(ctx, request, shape)
			},
			Baseline:  baseRequest(t),
			Supported: []inference.Field{inference.FieldContext, inference.FieldInput, inference.FieldRole, inference.FieldContent, inference.FieldOutput, inference.FieldTools, inference.FieldToolChoice, inference.FieldExtensions},
			Cases: []inferencetest.FieldCase{
				{Field: inference.FieldContext, Request: withContext, Marker: `"earlier"`},
				{Field: inference.FieldInput, Request: baseRequest(t), Marker: `"hello"`},
				{Field: inference.FieldRole, Request: baseRequest(t), Marker: `"role":"user"`},
				{Field: inference.FieldContent, Request: baseRequest(t), Marker: `"hello"`},
				{Field: inference.FieldOutput, Request: withTextOutput(t)},
				{Field: inference.FieldTools, Request: tools, Marker: `"name":"search"`},
				{Field: inference.FieldToolChoice, Request: withChoiceRequest, Marker: `"tool_choice"`},
				{Field: inference.FieldExtensions, Request: withOptions, Marker: `"temperature":0.3`},
			},
			Render: renderJSON[responses.ResponseNewParams],
			Rejections: []inferencetest.Rejection{
				{Name: "image output", Request: imageRequest(t, "a square"), Kind: inference.UnsupportedFeature, Field: inference.FieldOutput},
				{Name: "unknown extension", Request: withForeignExtension(t), Kind: inference.InvalidExtension, Field: inference.FieldExtensions},
			},
		})
	})

	t.Run("provider", func(t *testing.T) {
		inferencetest.RunProvider(t, inferencetest.ProviderFixture{
			ProviderID:  ProviderID,
			NewProvider: newProvider,
			Request:     chatRequestWithTools,
			Samples: map[inference.Output][]inferencetest.Sample{
				inference.OutputText: {
					{Name: "text", Body: responsesBody("hello there", "completed"),
						Check: func(t *testing.T, response inference.GenerateResponse) {
							if got := response.Message.Content.Text(); got != "hello there" {
								t.Fatalf("text = %q", got)
							}
							if response.FinishReason != inference.FinishStop {
								t.Fatalf("finish = %q", response.FinishReason)
							}
						}},
					{Name: "server error", Status: 500, Body: `{"error":{"message":"boom"}}`,
						WantKind: inference.ProviderFailure},
					{Name: "unknown status", Body: responsesBody("x", "banana"),
						WantKind: inference.ProviderResponseUnusable},
				},
			},
		})
	})

	t.Run("credentialed", func(t *testing.T) {
		inferencetest.RunCredentialed(t, inferencetest.CredentialedFixture{
			EnvKey: "OPENAI_API_KEY",
			NewProvider: func(t *testing.T) (inference.Provider, error) {
				return New(Settings{API: APIResponses, APIKey: apiKey(t)})
			},
			NewProviderWithKey: func(t *testing.T, key string) (inference.Provider, error) {
				return New(Settings{API: APIResponses, APIKey: key})
			},
			Request: func(t *testing.T, _ inference.Output) inference.GenerateRequest {
				return chatRequestWithTools(t)
			},
			Outputs: []inference.Output{inference.OutputText},
		})
	})
}

func TestImageConformance(t *testing.T) {
	d := &driver{settings: testSettings(APIChat)}

	t.Run("compiler", func(t *testing.T) {
		count := 3
		withCount := imageRequest(t, "a red square")
		withCount.Extensions = inference.Extensions{&ImageOptions{Count: &count}}

		withSize := imageRequest(t, "a red square")
		withSize.Extensions = inference.Extensions{&ImageOptions{Size: "1024x1024"}}

		withFormat := imageRequest(t, "a red square")
		withFormat.Extensions = inference.Extensions{&ImageOptions{ResponseFormat: "b64_json"}}

		withContext := imageRequest(t, "a red square")
		withContext.Context = []message.Message{message.NewTextMessage(message.RoleUser, "earlier")}

		inferencetest.RunCompiler(t, inferencetest.CompilerFixture[openai.ImageGenerateParams]{
			ProviderID: ProviderID,
			Compile: func(ctx context.Context, request inference.GenerateRequest, shape protocol.Shape) (protocol.Compiled[openai.ImageGenerateParams], error) {
				return d.imageCompile(ctx, request, shape)
			},
			Baseline:  imageRequest(t, "a red square"),
			Supported: []inference.Field{inference.FieldInput, inference.FieldRole, inference.FieldContent, inference.FieldOutput, inference.FieldExtensions, inference.Field("extension.openai.image.count"), inference.Field("extension.openai.image.size"), inference.Field("extension.openai.image.response_format")},
			Cases: []inferencetest.FieldCase{
				{Field: inference.FieldInput, Request: imageRequest(t, "a red square"), Marker: `"a red square"`},
				{Field: inference.FieldRole, Request: imageRequest(t, "a red square")},
				{Field: inference.FieldContent, Request: imageRequest(t, "a red square"), Marker: `"prompt"`},
				{Field: inference.FieldOutput, Request: imageRequest(t, "a red square"), Marker: `"model":"gpt-image-1"`},
				{Field: inference.FieldExtensions, Request: withCount, Marker: `"n":3`},
				{Field: inference.Field("extension.openai.image.count"), Request: withCount, Marker: `"n":3`},
				{Field: inference.Field("extension.openai.image.size"), Request: withSize, Marker: `"size":"1024x1024"`},
				{Field: inference.Field("extension.openai.image.response_format"), Request: withFormat, Marker: `"response_format":"b64_json"`},
			},
			Render: renderJSON[openai.ImageGenerateParams],
			Rejections: []inferencetest.Rejection{
				{Name: "conversation context", Request: withContext, Kind: inference.UnsupportedFeature, Field: inference.FieldContext},
				{Name: "text output", Request: chatRequestWithTools(t), Kind: inference.UnsupportedFeature, Field: inference.FieldOutput},
			},
		})
	})
}

// chatToolStreamBody streams one tool call in fragments: the call is announced,
// then its arguments arrive in pieces.
func chatToolStreamBody() string {
	return "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5-mini\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"search\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5-mini\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"q\\\":\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5-mini\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"go\\\"}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5-mini\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
}

// TestCatalogConformance covers the declaration surface. A deployment that
// declares a line-up publishes it and enforces it; one that declares nothing
// stays permissive and publishes no catalog.
func TestCatalogConformance(t *testing.T) {
	text := model.ModelDescriptor{
		ID: model.ModelID{Provider: ProviderID, Name: "gpt-5-mini"},
		Capabilities: model.ModelCapabilities{
			Inputs:      []message.PartKind{message.PartText},
			Outputs:     []message.PartKind{message.PartText},
			ToolCalling: true,
		},
		Limits: model.ModelLimits{MaxInputTokens: intPtr(128000)},
	}
	image := model.ModelDescriptor{
		ID:           model.ModelID{Provider: ProviderID, Name: "gpt-image-1"},
		Capabilities: model.ModelCapabilities{Outputs: []message.PartKind{message.PartImage}},
	}

	newProvider := func(t *testing.T, baseURL string) (inference.Provider, error) {
		return New(Settings{
			API: APIChat, APIKey: "test-key", BaseURL: baseURL,
			Models: []model.ModelDescriptor{text, image},
		})
	}

	inferencetest.RunCatalog(t, inferencetest.CatalogFixture{
		ProviderID:  ProviderID,
		NewProvider: newProvider,
		Declared:    []model.ModelDescriptor{text, image},
		Served:      []inference.Output{inference.OutputText, inference.OutputImage},
		Enforces:    true,
		UndeclaredInput: func(t *testing.T) (inference.GenerateRequest, message.PartKind) {
			source, err := media.NewAudioBytes([]byte("RIFF"), "audio/wav")
			if err != nil {
				t.Fatalf("audio source: %v", err)
			}
			request := baseRequest(t)
			request.Model.ID.Name = text.ID.Name
			request.Input.Content = message.Content{Parts: []message.Part{
				message.TextPart{Text: "hello"},
				message.AudioPart{Source: source},
			}}
			return request, message.PartAudio
		},
		Request: func(t *testing.T, name string, output inference.Output) inference.GenerateRequest {
			if output == inference.OutputImage {
				request := imageRequest(t, "a red square")
				request.Model.ID.Name = name
				return request
			}
			request := baseRequest(t)
			request.Model.ID.Name = name
			return request
		},
	})

	t.Run("no declarations means no catalog", func(t *testing.T) {
		provider, err := New(Settings{API: APIChat, APIKey: "test-key"})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		if _, ok := provider.(inference.Catalog); ok {
			t.Fatal("a provider without declarations published a catalog")
		}
	})
}

func intPtr(value int) *int { return &value }

// TestDeclaredInputsAreEnforced covers the rule directly, including the case the
// suite cannot express: the same request compiles once the input is declared.
func TestDeclaredInputsAreEnforced(t *testing.T) {
	source, err := media.NewAudioBytes([]byte("RIFF"), "audio/wav")
	if err != nil {
		t.Fatalf("audio source: %v", err)
	}
	request := baseRequest(t)
	request.Model.ID.Name = "gpt-5-mini"
	request.Input.Content = message.Content{Parts: []message.Part{
		message.TextPart{Text: "hello"},
		message.AudioPart{Source: source},
	}}

	textOnly := model.ModelDescriptor{
		ID:           model.ModelID{Provider: ProviderID, Name: "gpt-5-mini"},
		Capabilities: model.ModelCapabilities{Inputs: []message.PartKind{message.PartText}},
	}
	d := &driver{settings: Settings{API: APIChat, APIKey: "k", Models: []model.ModelDescriptor{textOnly}}}
	_, compileErr := d.chatCompile(context.Background(), request, protocol.ShapeUnary)
	if !inference.IsKind(compileErr, inference.UnsupportedFeature) {
		t.Fatalf("kind = %v, want UnsupportedFeature", compileErr)
	}
	var inferenceErr *inference.Error
	if !errors.As(compileErr, &inferenceErr) {
		t.Fatalf("error is %T, want *inference.Error", compileErr)
	}
	if inferenceErr.Field != inference.FieldContent {
		t.Fatalf("field = %q, want content", inferenceErr.Field)
	}

	// Declaring the input is enough: the same request then compiles.
	acceptsAudio := textOnly
	acceptsAudio.Capabilities.Inputs = []message.PartKind{message.PartText, message.PartAudio}
	d = &driver{settings: Settings{API: APIChat, APIKey: "k", Models: []model.ModelDescriptor{acceptsAudio}}}
	if _, err := d.chatCompile(context.Background(), request, protocol.ShapeUnary); err != nil {
		t.Fatalf("declared audio rejected: %v", err)
	}

	// An empty input list declares nothing and filters nothing.
	undeclared := textOnly
	undeclared.Capabilities.Inputs = nil
	d = &driver{settings: Settings{API: APIChat, APIKey: "k", Models: []model.ModelDescriptor{undeclared}}}
	if _, err := d.chatCompile(context.Background(), request, protocol.ShapeUnary); err != nil {
		t.Fatalf("permissive model rejected audio: %v", err)
	}
}
