package bytedance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

func simpleTextRequest(text string) inference.GenerateRequest {
	return inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: inference.Content{
					Parts: []inference.Part{inference.TextPart{Text: text}},
				},
				Intent: inference.Intent{Text: &inference.TextIntent{}},
			},
		},
	}
}

func toolCallOutputItem() map[string]any {
	return map[string]any{
		"type":      "function_call",
		"call_id":   "call_9",
		"name":      "lookup",
		"arguments": `{"q":"ark"}`,
	}
}

func TestGenerateUnaryToolCalls(t *testing.T) {
	server, _ := newCapturedArk(t, func(w http.ResponseWriter, _ map[string]any, _ bool) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, responsesResponseJSON([]map[string]any{
			toolCallOutputItem(),
		}))
	})
	defer server.Close()
	runtime := newTestRuntime(t, server)

	request := simpleTextRequest("find something")
	request.Input.Content.Intent.Tools = &inference.ToolsIntent{
		Definitions: []tool.Definition{{
			Name:        "lookup",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	}
	response, err := runtime.Generate(
		context.Background(),
		generateModel("doubao-seed-2-1-pro"),
		request,
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.FinishReason != inference.FinishToolCalls {
		t.Fatalf("finish = %q", response.FinishReason)
	}
	call, ok := response.Message.Content.Parts[0].(inference.ToolCallPart)
	if !ok {
		t.Fatalf("part = %#v", response.Message.Content.Parts[0])
	}
	if call.Call.ID != "call_9" || call.Call.Name != "lookup" ||
		string(call.Call.Arguments) != `{"q":"ark"}` {
		t.Fatalf("call = %+v", call.Call)
	}
}

// sseBody renders a responses-API SSE fixture: one data line per event.
func sseBody(events ...map[string]any) string {
	body := ""
	for _, event := range events {
		payload, _ := json.Marshal(event)
		body += "data: " + string(payload) + "\n\n"
	}
	return body
}

func TestGenerateStreamCapturedWire(t *testing.T) {
	server, capture := newCapturedArk(t, func(w http.ResponseWriter, _ map[string]any, stream bool) {
		if !stream {
			t.Error("stream request did not carry stream: true")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseBody(
			map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message"}},
			map[string]any{"type": "response.output_text.delta", "output_index": 0, "delta": "hel"},
			map[string]any{"type": "response.output_text.delta", "output_index": 0, "delta": "lo"},
			map[string]any{
				"type": "response.output_item.added", "output_index": 1,
				"item": map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup"},
			},
			map[string]any{"type": "response.function_call_arguments.delta", "output_index": 1, "delta": `{"q":`},
			map[string]any{"type": "response.function_call_arguments.delta", "output_index": 1, "delta": `"x"}`},
			map[string]any{
				"type":     "response.completed",
				"response": map[string]any{
					"id": "resp_1", "status": "completed",
					"usage": map[string]any{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8},
				},
			},
		))
	})
	defer server.Close()
	runtime := newTestRuntime(t, server)

	request := simpleTextRequest("hi")
	request.Input.Content.Intent.Tools = &inference.ToolsIntent{
		Definitions: []tool.Definition{{
			Name:        "lookup",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	}
	stream, err := runtime.GenerateStream(
		context.Background(),
		generateModel("doubao-seed-2-1-pro"),
		request,
	)
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer stream.Close()

	var textDeltas int
	var toolFragments []string
	for {
		event, err := stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			for depth, current := 0, err; current != nil && depth < 8; depth++ {
				t.Logf("depth %d: %T: %v", depth, current, current)
				current = errors.Unwrap(current)
			}
			t.Fatalf("Next: %v", err)
		}
		switch delta := event.Delta.(type) {
		case inference.TextPartDelta:
			textDeltas++
		case inference.ToolCallDelta:
			toolFragments = append(toolFragments, delta.ArgumentsFragment)
		}
		if event.FinishReason != "" && event.FinishReason != inference.FinishToolCalls {
			t.Fatalf("finish = %q", event.FinishReason)
		}
	}
	result, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if textDeltas != 2 {
		t.Fatalf("text deltas = %d", textDeltas)
	}
	wantFragments := []string{"", `{"q":`, `"x"}`}
	if len(toolFragments) != len(wantFragments) {
		t.Fatalf("tool fragments = %q", toolFragments)
	}
	for index, want := range wantFragments {
		if toolFragments[index] != want {
			t.Fatalf("tool fragment %d = %q, want %q", index, toolFragments[index], want)
		}
	}
	if result.FinishReason != inference.FinishToolCalls {
		t.Fatalf("result finish = %q", result.FinishReason)
	}
	parts := result.Message.Content.Parts
	text, ok := parts[0].(inference.TextPart)
	if !ok || text.Text != "hello" {
		t.Fatalf("text part = %#v", parts[0])
	}
	call, ok := parts[1].(inference.ToolCallPart)
	if !ok || call.Call.ID != "call_1" || call.Call.Name != "lookup" ||
		string(call.Call.Arguments) != `{"q":"x"}` {
		t.Fatalf("tool part = %#v", parts[1])
	}
	if result.Usage.TotalTokens != 8 {
		t.Fatalf("usage = %+v", result.Usage)
	}
	if capture.body(0)["stream"] != true {
		t.Fatalf("stream flag = %v", capture.body(0)["stream"])
	}
}

func TestGenerateErrorClassification(t *testing.T) {
	server, _ := newCapturedArk(t, func(w http.ResponseWriter, _ map[string]any, _ bool) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"code":"rate_limit","message":"slow down","type":"rate_limit","request_id":"r1"}}`)
	})
	defer server.Close()
	runtime := newTestRuntime(t, server)
	_, err := runtime.Generate(
		context.Background(),
		generateModel("doubao-seed-2-1-pro"),
		simpleTextRequest("hi"),
	)
	if err == nil {
		t.Fatal("expected provider failure")
	}
	if !inference.IsKind(err, inference.ProviderFailure) {
		t.Fatalf("kind = %v", err)
	}
	if !errdefs.HasClassification(errdefs.FromContext(err)) {
		t.Fatalf("classification lost: %v", err)
	}
}
