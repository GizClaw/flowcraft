package anthropic

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
)

func TestGenerationOptionsApply(t *testing.T) {
	temperature := 0.2
	topP := 0.9
	topK := 40
	maxTokens := 4096
	budget := 2048
	request := testRequest(t)
	request.Extensions = inference.Extensions{&GenerationOptions{
		Temperature:     &temperature,
		TopP:            &topP,
		TopK:            &topK,
		MaxOutputTokens: &maxTokens,
		Thinking:        &Thinking{BudgetTokens: &budget},
		ResponseFormat: &ResponseFormat{
			Schema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`),
		},
	}}
	if err := request.Validate(); err != nil {
		t.Fatalf("request: %v", err)
	}

	compiled, err := testDriver(Settings{APIKey: "key"}).compile(
		context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	wire := compiled.Wire
	if wire.MaxTokens != 4096 {
		t.Fatalf("max tokens = %d", wire.MaxTokens)
	}
	if !wire.Temperature.Valid() || wire.Temperature.Value != 0.2 {
		t.Fatalf("temperature = %+v", wire.Temperature)
	}
	if !wire.TopP.Valid() || wire.TopP.Value != 0.9 {
		t.Fatalf("top_p = %+v", wire.TopP)
	}
	if !wire.TopK.Valid() || wire.TopK.Value != 40 {
		t.Fatalf("top_k = %+v", wire.TopK)
	}
	if wire.Thinking.OfEnabled == nil || wire.Thinking.OfEnabled.BudgetTokens != 2048 {
		t.Fatalf("thinking = %+v", wire.Thinking)
	}
	if wire.OutputConfig.Format.Schema["type"] != "object" {
		t.Fatalf("format schema = %+v", wire.OutputConfig.Format.Schema)
	}
}

func TestGenerationOptionsThinkingControls(t *testing.T) {
	enabled := true
	disabled := false
	effort := EffortHigh
	for name, thinking := range map[string]*Thinking{
		"adaptive": {Enabled: &enabled},
		"disabled": {Enabled: &disabled},
		"effort":   {Effort: effort},
	} {
		t.Run(name, func(t *testing.T) {
			request := testRequest(t)
			request.Extensions = inference.Extensions{&GenerationOptions{Thinking: thinking}}
			if err := request.Validate(); err != nil {
				t.Fatalf("request: %v", err)
			}
			compiled, err := testDriver(Settings{APIKey: "key"}).compile(
				context.Background(), request, protocol.ShapeUnary)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			switch name {
			case "adaptive":
				if compiled.Wire.Thinking.OfAdaptive == nil {
					t.Fatalf("thinking = %+v, want adaptive", compiled.Wire.Thinking)
				}
			case "disabled":
				if compiled.Wire.Thinking.OfDisabled == nil {
					t.Fatalf("thinking = %+v, want disabled", compiled.Wire.Thinking)
				}
			case "effort":
				if compiled.Wire.OutputConfig.Effort != "high" {
					t.Fatalf("effort = %q", compiled.Wire.OutputConfig.Effort)
				}
			}
		})
	}
}

func TestGenerationOptionsRejectABudgetAtOrAboveMax(t *testing.T) {
	budget := DefaultMaxTokens
	request := testRequest(t)
	request.Extensions = inference.Extensions{&GenerationOptions{
		Thinking: &Thinking{BudgetTokens: &budget},
	}}
	if err := request.Validate(); err != nil {
		t.Fatalf("request: %v", err)
	}
	_, err := testDriver(Settings{APIKey: "key"}).compile(
		context.Background(), request, protocol.ShapeUnary)
	if err == nil || !inference.IsKind(err, inference.InvalidExtension) {
		t.Fatalf("error = %v, want InvalidExtension", err)
	}
}

func TestGenerationDecoderIsStrict(t *testing.T) {
	decode := Decoders()[ProviderID+"/"+GenerationExtensionID]
	if decode == nil {
		t.Fatal("generation decoder is not registered")
	}
	extension, err := decode(json.RawMessage(`{"temperature":0.2,"top_k":40}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	options, ok := extension.(*GenerationOptions)
	if !ok || options.Temperature == nil || *options.Temperature != 0.2 {
		t.Fatalf("decoded = %#v", extension)
	}
	_, err = decode(json.RawMessage(`{"temperature":0.2,"typo":true}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v, want an unknown-field error", err)
	}
}
