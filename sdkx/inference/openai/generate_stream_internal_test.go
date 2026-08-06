package openai

import (
	"testing"

	"github.com/openai/openai-go/responses"
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
			Arguments: args,
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
