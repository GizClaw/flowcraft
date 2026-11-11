package anthropic

import (
	"context"
	"encoding/json"
	"testing"

	anthropicgo "github.com/anthropics/anthropic-sdk-go"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

type foreignExtension struct{}

func (foreignExtension) ProviderID() string  { return "other" }
func (foreignExtension) ExtensionID() string { return "foreign" }
func (foreignExtension) ActiveFields() []inference.ExtensionField {
	return []inference.ExtensionField{"value"}
}
func (foreignExtension) Validate() error            { return nil }
func (foreignExtension) Clone() inference.Extension { return foreignExtension{} }

func withTools(t *testing.T) inference.GenerateRequest {
	t.Helper()
	request := testRequest(t)
	tools, err := inference.NewToolset([]message.ToolDefinition{{
		Name:        "search",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}})
	if err != nil {
		t.Fatalf("toolset: %v", err)
	}
	request.Tools = tools
	return request
}

func withChoice(t *testing.T) inference.GenerateRequest {
	t.Helper()
	request := withTools(t)
	choice, err := request.Tools.Choosing(inference.ToolChoice{Mode: inference.ToolChoiceAuto})
	if err != nil {
		t.Fatalf("choosing: %v", err)
	}
	request.Tools = choice
	return request
}

func withContext(t *testing.T) inference.GenerateRequest {
	t.Helper()
	request := testRequest(t)
	request.Context = []message.Message{
		message.NewTextMessage(message.RoleUser, "earlier"),
	}
	return request
}

func withOptions(t *testing.T) inference.GenerateRequest {
	t.Helper()
	temperature := 0.3
	request := testRequest(t)
	request.Extensions = inference.Extensions{
		&GenerationOptions{Temperature: &temperature},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("request: %v", err)
	}
	return request
}

func withForeignExtension(t *testing.T) inference.GenerateRequest {
	t.Helper()
	request := testRequest(t)
	request.Extensions = inference.Extensions{foreignExtension{}}
	if err := request.Validate(); err != nil {
		t.Fatalf("request: %v", err)
	}
	return request
}

func renderMessage(params anthropicgo.MessageNewParams) (string, error) {
	raw, err := json.Marshal(params)
	return string(raw), err
}

func TestConformance(t *testing.T) {
	d := testDriver(Settings{APIKey: "test-key"})
	newProvider := func(t *testing.T, baseURL string) (inference.Provider, error) {
		return New(Settings{BaseURL: baseURL, APIKey: "test-key"})
	}

	t.Run("compiler", func(t *testing.T) {
		inferencetest.RunCompiler(t, inferencetest.CompilerFixture[anthropicgo.MessageNewParams]{
			ProviderID: ProviderID,
			Compile: func(
				ctx context.Context,
				request inference.GenerateRequest,
				shape protocol.Shape,
			) (protocol.Compiled[anthropicgo.MessageNewParams], error) {
				return d.compile(ctx, request, shape)
			},
			Baseline: testRequest(t),
			Supported: []inference.Field{
				inference.FieldContext,
				inference.FieldInput,
				inference.FieldRole,
				inference.FieldContent,
				inference.FieldOutput,
				inference.FieldTools,
				inference.FieldToolChoice,
				inference.FieldExtensions,
			},
			Cases: []inferencetest.FieldCase{
				{Field: inference.FieldContext, Request: withContext(t), Marker: `"earlier"`},
				{Field: inference.FieldInput, Request: testRequest(t), Marker: `"hello"`},
				{Field: inference.FieldRole, Request: testRequest(t), Marker: `"role":"user"`},
				{Field: inference.FieldContent, Request: testRequest(t), Marker: `"hello"`},
				{Field: inference.FieldOutput, Request: testRequest(t)},
				{Field: inference.FieldTools, Request: withTools(t), Marker: `"name":"search"`},
				{Field: inference.FieldToolChoice, Request: withChoice(t), Marker: `"tool_choice"`},
				{Field: inference.FieldExtensions, Request: withOptions(t), Marker: `"temperature":0.3`},
			},
			Render: renderMessage,
			Rejections: []inferencetest.Rejection{
				{Name: "image output", Request: outputImage(t), Kind: inference.UnsupportedFeature, Field: inference.FieldOutput},
				{Name: "unknown extension", Request: withForeignExtension(t), Kind: inference.InvalidExtension, Field: inference.FieldExtensions},
			},
		})
	})

	t.Run("provider", func(t *testing.T) {
		inferencetest.RunGenerator(t, inferencetest.ProviderFixture{
			ProviderID:   ProviderID,
			NewGenerator: newProvider,
			Request:      testRequest,
			Samples: map[inference.Output][]inferencetest.Sample{
				inference.OutputText: {
					{Name: "text", Body: textBody(`"hello"`, "end_turn"), Check: func(t *testing.T, response inference.GenerateResponse) {
						if got := response.Message.Content.Text(); got != "hello" {
							t.Fatalf("text = %q", got)
						}
						if response.FinishReason != inference.FinishStop {
							t.Fatalf("finish = %q", response.FinishReason)
						}
						if response.Usage.InputTokens != 4 || response.Usage.OutputTokens != 2 {
							t.Fatalf("usage = %+v", response.Usage)
						}
					}},
					{Name: "undefined tool call", Body: toolBody(), WantKind: inference.UndefinedTool},
					{Name: "server error", Status: 503, Body: `{"type":"error","error":{"type":"api_error","message":"down"}}`, WantKind: inference.ProviderFailure},
					{Name: "unknown finish reason", Body: textBody(`"x"`, "banana"), WantKind: inference.ProviderResponseUnusable},
				},
			},
		})
	})

	t.Run("stream", func(t *testing.T) {
		inferencetest.RunStream(t, inferencetest.StreamFixture{
			ProviderID:   ProviderID,
			NewGenerator: newProvider,
			Request:      testRequest,
			Samples: []inferencetest.StreamSample{
				{
					Name: "text",
					Body: streamBody("hello"),
					WantKinds: []inference.StreamEventKind{
						inference.StreamStart,
						inference.StreamText,
						inference.StreamUsage,
						inference.StreamEnd,
					},
					WantText:  "hello",
					WantUsage: &inferencetest.UsageExpectation{Input: 4, Output: 2, Total: 6},
					WantParts: []inferencetest.PartExpectation{{Kind: message.PartText, Text: "hello"}},
				},
			},
			Truncated: &inferencetest.StreamSample{
				Name: "no terminal event",
				Body: truncatedStreamBody(),
			},
			Equivalence: &inferencetest.Equivalence{
				Unary: inferencetest.ProviderFixture{
					ProviderID:   ProviderID,
					NewGenerator: newProvider,
					Request:      testRequest,
				},
				UnarySample: inferencetest.Sample{Name: "unary", Body: textBody(`"hello"`, "end_turn")},
				Stream:      inferencetest.StreamSample{Name: "equivalence", Body: streamBody("hello")},
				Compare: func(t *testing.T, unary, streamed inference.GenerateResponse) {
					if unary.Message.Content.Text() != streamed.Message.Content.Text() {
						t.Fatalf("text: unary %q vs streamed %q",
							unary.Message.Content.Text(), streamed.Message.Content.Text())
					}
					if unary.FinishReason != streamed.FinishReason {
						t.Fatalf("finish: %q vs %q", unary.FinishReason, streamed.FinishReason)
					}
					if unary.Usage.TotalTokens() != streamed.Usage.TotalTokens() {
						t.Fatalf("usage: %+v vs %+v", unary.Usage, streamed.Usage)
					}
				},
			},
		})
	})
}

func outputImage(t *testing.T) inference.GenerateRequest {
	t.Helper()
	request := testRequest(t)
	request.Output = inference.OutputImage
	if err := request.Validate(); err != nil {
		t.Fatalf("request: %v", err)
	}
	return request
}

func textBody(text, stop string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test",` +
		`"content":[{"type":"text","text":` + text + `}],` +
		`"stop_reason":"` + stop + `",` +
		`"usage":{"input_tokens":4,"output_tokens":2}}`
}

func toolBody() string {
	return `{"id":"msg_2","type":"message","role":"assistant","model":"claude-test",` +
		`"content":[{"type":"tool_use","id":"toolu_1","name":"missing","input":{}}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":4,"output_tokens":1}}`
}

func streamBody(text string) string {
	return `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"usage":{"input_tokens":4,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + text + `"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`
}

func truncatedStreamBody() string {
	return `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"usage":{"input_tokens":4,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}

`
}

func TestCatalogConformance(t *testing.T) {
	descriptor := model.ModelDescriptor{
		ID: model.ModelID{Provider: ProviderID, Name: "claude-test"},
		Capabilities: model.ModelCapabilities{
			Inputs:      []message.PartKind{message.PartText},
			Outputs:     []message.PartKind{message.PartText},
			ToolCalling: true,
		},
	}
	newProvider := func(t *testing.T, baseURL string) (inference.Provider, error) {
		return New(Settings{
			BaseURL: baseURL, APIKey: "test-key",
			Models: []model.ModelDescriptor{descriptor},
		})
	}

	inferencetest.RunCatalog(t, inferencetest.CatalogFixture{
		ProviderID:   ProviderID,
		NewGenerator: newProvider,
		Declared:     []model.ModelDescriptor{descriptor},
		Served:       []inference.Output{inference.OutputText},
		Enforces:     true,
		UndeclaredInput: func(t *testing.T) (inference.GenerateRequest, message.PartKind) {
			source, err := media.NewAudioBytes([]byte("RIFF"), "audio/wav")
			if err != nil {
				t.Fatalf("audio source: %v", err)
			}
			request := testRequest(t)
			request.Input.Content = message.Content{Parts: []message.Part{
				message.TextPart{Text: "hello"},
				message.AudioPart{Source: source},
			}}
			return request, message.PartAudio
		},
		Request: func(t *testing.T, name string, _ inference.Output) inference.GenerateRequest {
			request := testRequest(t)
			request.Model.ID.Name = name
			return request
		},
	})
}
