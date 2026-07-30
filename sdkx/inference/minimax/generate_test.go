package minimax

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/media"
)

// Generate pipeline behavior tests, run end to end through the runtime
// against the fake Messages endpoint: the binary-thinking dialect, the
// thinking defaults, the signed thinking round-trip, capability gating,
// streaming, and error classification.

// TestAdaptiveThinkingOnWire asserts the binary-thinking dialect: any
// requested reasoning effort compiles to thinking: {type: "adaptive"},
// never output_config.effort.
func TestAdaptiveThinkingOnWire(t *testing.T) {
	server := newMessagesServer(t, func(w http.ResponseWriter, _ map[string]any) {
		fmt.Fprint(w, messageJSON([]map[string]any{
			{"type": "text", "text": "ok"},
		}))
	})
	runtime := newTestRuntime(t, server)

	request := simpleTextRequest("hi")
	request.Input.Content.Intent.Text.ReasoningEffort = inference.ReasoningLow
	response, err := runtime.Generate(context.Background(), minimaxModel("MiniMax-M3"), request)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(response.Message.Content.Parts) != 1 {
		t.Fatalf("parts = %d", len(response.Message.Content.Parts))
	}

	body := server.body(t, 0)
	thinking, ok := body["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("captured body has no thinking param: %v", body)
	}
	if thinking["type"] != "adaptive" {
		t.Fatalf("thinking.type = %v", thinking["type"])
	}
	if _, exists := body["output_config"]; exists {
		t.Fatalf("binary dialect must not emit output_config: %v", body)
	}
}

// TestNoThinkingByDefault asserts a request without a reasoning intent
// sends no thinking param — MiniMax-M3 keeps thinking off by default.
func TestNoThinkingByDefault(t *testing.T) {
	server := newMessagesServer(t, func(w http.ResponseWriter, _ map[string]any) {
		fmt.Fprint(w, messageJSON([]map[string]any{
			{"type": "text", "text": "ok"},
		}))
	})
	runtime := newTestRuntime(t, server)

	if _, err := runtime.Generate(context.Background(), minimaxModel("MiniMax-M3"), simpleTextRequest("hi")); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	body := server.body(t, 0)
	if _, exists := body["thinking"]; exists {
		t.Fatalf("thinking param must stay unset: %v", body)
	}
}

// TestSignedThinkingRoundTrip asserts a thinking block decodes into a
// reasoning part carrying its signature, and that the context compile
// round-trips the block hoisted first with the signature intact —
// MiniMax requires preserved thinking blocks across tool-use turns.
func TestSignedThinkingRoundTrip(t *testing.T) {
	server := newMessagesServer(t, func(w http.ResponseWriter, body map[string]any) {
		if messages, hasMessages := body["messages"].([]any); hasMessages {
			// Any assistant message in the context must open with the
			// thinking block, signature intact.
			for _, raw := range messages {
				message := raw.(map[string]any)
				if message["role"] != "assistant" {
					continue
				}
				content := message["content"].([]any)
				first := content[0].(map[string]any)
				if first["type"] != "thinking" || first["signature"] != "sig-abc" {
					t.Errorf("thinking block did not round-trip: %v", content)
				}
			}
		}
		fmt.Fprint(w, messageJSON([]map[string]any{
			{"type": "thinking", "thinking": "reasoning about it", "signature": "sig-abc"},
			{"type": "text", "text": "answer"},
		}))
	})
	runtime := newTestRuntime(t, server)

	response, err := runtime.Generate(context.Background(), minimaxModel("MiniMax-M3"), simpleTextRequest("hi"))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	parts := response.Message.Content.Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want reasoning + text", len(parts))
	}
	reasoning, ok := parts[0].(inference.ReasoningPart)
	if !ok {
		t.Fatalf("parts[0] = %#v, want ReasoningPart", parts[0])
	}
	if reasoning.Text != "reasoning about it" || reasoning.Signature != "sig-abc" {
		t.Fatalf("reasoning = %+v", reasoning)
	}

	followUp := simpleTextRequest("next")
	followUp.Context = []inference.Message{
		{Role: inference.RoleUser, Content: inference.Content{
			Parts: []inference.Part{inference.TextPart{Text: "hi"}},
		}},
		{Role: inference.RoleAssistant, Content: inference.Content{
			Parts: parts,
		}},
	}
	if _, err := runtime.Generate(context.Background(), minimaxModel("MiniMax-M3"), followUp); err != nil {
		t.Fatalf("Generate follow-up: %v", err)
	}
}

// TestVisionGating asserts image input compiles on MiniMax-M3 and rejects
// on the text-only M2.x series.
func TestVisionGating(t *testing.T) {
	server := newMessagesServer(t, func(w http.ResponseWriter, _ map[string]any) {
		fmt.Fprint(w, messageJSON([]map[string]any{
			{"type": "text", "text": "ok"},
		}))
	})
	runtime := newTestRuntime(t, server)

	image, err := media.NewImageURL("https://example.com/cat.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	imageRequest := inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: inference.Content{Parts: []inference.Part{
					inference.ImagePart{Source: image},
					inference.TextPart{Text: "what is this?"},
				}},
				Intent: inference.Intent{Text: &inference.TextIntent{}},
			},
		},
	}

	if _, err := runtime.Generate(context.Background(), minimaxModel("MiniMax-M3"), imageRequest); err != nil {
		t.Fatalf("M3 image Generate: %v", err)
	}
	body := server.body(t, 0)
	messages := body["messages"].([]any)
	content := messages[0].(map[string]any)["content"].([]any)
	if content[0].(map[string]any)["type"] != "image" {
		t.Fatalf("compiled content = %v", content)
	}

	_, err = runtime.Generate(context.Background(), minimaxModel("MiniMax-M2.7"), imageRequest)
	if err == nil {
		t.Fatal("M2.7 image Generate must reject")
	}
	if !inference.IsKind(err, inference.UnsupportedFeature) {
		t.Fatalf("err = %v, want UnsupportedFeature", err)
	}
}

// TestStreamDecodesThinking asserts the stream surface surfaces thinking
// deltas with their signature as reasoning parts.
func TestStreamDecodesThinking(t *testing.T) {
	server := newMessagesServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseBody(
			map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id": "msg_1", "type": "message", "role": "assistant",
					"model": "MiniMax-M3", "content": []any{},
					"usage": map[string]any{"input_tokens": 12, "output_tokens": 0},
				},
			},
			map[string]any{
				"type":          "content_block_start",
				"index":         0,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			},
			map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "thinking_delta", "thinking": "let me think"},
			},
			map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "signature_delta", "signature": "sig-stream"},
			},
			map[string]any{"type": "content_block_stop", "index": 0},
			map[string]any{
				"type":          "content_block_start",
				"index":         1,
				"content_block": map[string]any{"type": "text", "text": ""},
			},
			map[string]any{
				"type":  "content_block_delta",
				"index": 1,
				"delta": map[string]any{"type": "text_delta", "text": "done"},
			},
			map[string]any{"type": "content_block_stop", "index": 1},
			map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": "end_turn"},
				"usage": map[string]any{"output_tokens": 7},
			},
			map[string]any{"type": "message_stop"},
		))
	})
	runtime := newTestRuntime(t, server)

	stream, err := runtime.GenerateStream(context.Background(), minimaxModel("MiniMax-M3"), simpleTextRequest("hi"))
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	for {
		_, err = stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	response, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	parts := response.Message.Content.Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want reasoning + text", len(parts))
	}
	reasoning, ok := parts[0].(inference.ReasoningPart)
	if !ok || reasoning.Text != "let me think" || reasoning.Signature != "sig-stream" {
		t.Fatalf("reasoning = %#v", parts[0])
	}
	text, ok := parts[1].(inference.TextPart)
	if !ok || text.Text != "done" {
		t.Fatalf("text = %#v", parts[1])
	}
}

// TestErrorClassification asserts HTTP failures classify through the
// anthropic kernel's taxonomy: a 429 surfaces as rate-limited.
func TestErrorClassification(t *testing.T) {
	server := newMessagesServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	})
	runtime := newTestRuntime(t, server)

	_, err := runtime.Generate(context.Background(), minimaxModel("MiniMax-M3"), simpleTextRequest("hi"))
	if err == nil || !strings.Contains(err.Error(), "rate") {
		t.Fatalf("err = %v", err)
	}
}
