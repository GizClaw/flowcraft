package openai

import (
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3/responses"
)

func TestResponsesStreamToolArgumentsSnapshotDeduplicated(t *testing.T) {
	s := &responsesStream{parts: make(map[int64]*streamPart)}
	const args = `{"city":"beijing"}`

	// No incremental deltas streamed: the arguments.done snapshot carries
	// the full payload.
	first := responses.ResponseStreamEventUnion{
		Type:        "response.function_call_arguments.done",
		OutputIndex: 0,
		Arguments:   args,
	}
	raw, keep, err := s.apply(first)
	if err != nil {
		t.Fatalf("apply arguments.done: %v", err)
	}
	if !keep || raw.kind != streamRawToolFragment || raw.tool.argsFragment != args {
		t.Fatalf("arguments.done raw = %+v keep=%v, want full arguments", raw, keep)
	}

	// output_item.done repeats the same full arguments; it must not append
	// them a second time.
	second := responses.ResponseStreamEventUnion{
		Type:        "response.output_item.done",
		OutputIndex: 0,
		Item: responses.ResponseOutputItemUnion{
			Type:      "function_call",
			ID:        "call-1",
			CallID:    "call-1",
			Name:      "weather",
			Arguments: responses.ResponseOutputItemUnionArguments{OfString: args},
		},
	}
	raw, keep, err = s.apply(second)
	if err != nil {
		t.Fatalf("apply output_item.done: %v", err)
	}
	if !keep {
		t.Fatal("output_item.done should still emit the tool identity")
	}
	if raw.tool.argsFragment != "" {
		t.Fatalf("output_item.done re-emitted arguments: %q", raw.tool.argsFragment)
	}
	if raw.tool.id != "call-1" || raw.tool.name != "weather" {
		t.Fatalf("output_item.done tool identity = %+v, want call-1/weather", raw.tool)
	}
}

func TestResponsesStreamWebSearchProviderOutputSnapshot(t *testing.T) {
	s := &responsesStream{parts: make(map[int64]*streamPart)}
	var event responses.ResponseStreamEventUnion
	if err := json.Unmarshal([]byte(`{
		"type":"response.output_item.done",
		"output_index":0,
		"item":{
			"type":"web_search_call",
			"id":"ws_1",
			"status":"completed",
			"action":{
				"type":"search",
				"queries":["flowcraft"],
				"sources":[{"type":"url","url":"https://example.com"}]
			}
		}
	}`), &event); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	raw, keep, err := s.apply(event)
	if err != nil {
		t.Fatalf("apply web_search_call done: %v", err)
	}
	if !keep || raw.kind != streamRawProviderOutput || len(raw.providerOutputs) != 1 {
		t.Fatalf("raw = %+v keep=%v, want provider output snapshot", raw, keep)
	}
	output, ok := raw.providerOutputs[0].(*WebSearchOutput)
	if !ok {
		t.Fatalf("output type = %T", raw.providerOutputs[0])
	}
	if len(output.Calls) != 1 || output.Calls[0].ID != "ws_1" ||
		len(output.Calls[0].Sources) != 1 ||
		output.Calls[0].Sources[0] != "https://example.com" {
		t.Fatalf("output = %+v", output)
	}
}
