package protocol

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// assemble runs the accumulator over events and returns the finished response.
func assemble(t *testing.T, events ...inference.StreamEvent) inference.GenerateResponse {
	t.Helper()
	acc := newAccumulator()
	acc.add(events)
	if !acc.finished() {
		t.Fatal("events carried no terminal event")
	}
	response, err := acc.response()
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	return response
}

func textOf(t *testing.T, response inference.GenerateResponse) string {
	t.Helper()
	return response.Message.Content.Text()
}

func TestAssemblyConcatenatesTextFragments(t *testing.T) {
	response := assemble(t,
		inference.StreamEvent{Kind: inference.StreamText, Index: 0, Text: "hel"},
		inference.StreamEvent{Kind: inference.StreamText, Index: 0, Text: "lo "},
		inference.StreamEvent{Kind: inference.StreamText, Index: 0, Text: "world"},
		inference.StreamEvent{Kind: inference.StreamEnd, Finish: inference.FinishStop},
	)
	if got := textOf(t, response); got != "hello world" {
		t.Fatalf("text = %q", got)
	}
}

// TestAssemblyOrdersPartsByIndex pins that arrival order does not decide content
// order: providers may interleave indices, and the part order is what a caller
// reads.
func TestAssemblyOrdersPartsByIndex(t *testing.T) {
	response := assemble(t,
		inference.StreamEvent{Kind: inference.StreamText, Index: 1, Text: "second"},
		inference.StreamEvent{Kind: inference.StreamText, Index: 0, Text: "first"},
		inference.StreamEvent{Kind: inference.StreamEnd, Finish: inference.FinishStop},
	)
	parts := response.Message.Content.Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %d", len(parts))
	}
	if first, ok := parts[0].(message.TextPart); !ok || first.Text != "first" {
		t.Fatalf("part 0 = %+v", parts[0])
	}
	if second, ok := parts[1].(message.TextPart); !ok || second.Text != "second" {
		t.Fatalf("part 1 = %+v", parts[1])
	}
}

// TestAssemblyAccumulatesToolCallFragments covers the shape every provider uses:
// the call is announced first, then its arguments arrive in pieces.
func TestAssemblyAccumulatesToolCallFragments(t *testing.T) {
	response := assemble(t,
		inference.StreamEvent{Kind: inference.StreamToolCall, Index: 0,
			Tool: &inference.ToolCallDelta{ID: "call_1", Name: "search"}},
		inference.StreamEvent{Kind: inference.StreamToolCall, Index: 0,
			Tool: &inference.ToolCallDelta{Arguments: `{"q":`}},
		inference.StreamEvent{Kind: inference.StreamToolCall, Index: 0,
			Tool: &inference.ToolCallDelta{Arguments: `"go"}`}},
		inference.StreamEvent{Kind: inference.StreamEnd, Finish: inference.FinishToolCalls},
	)
	calls := response.Message.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Name != "search" {
		t.Fatalf("call = %+v", calls[0])
	}
	if got := string(calls[0].Arguments); got != `{"q":"go"}` {
		t.Fatalf("arguments = %q", got)
	}
}

// TestAssemblyDefaultsEmptyArguments pins that a call with no arguments is an
// empty object rather than invalid JSON.
func TestAssemblyDefaultsEmptyArguments(t *testing.T) {
	response := assemble(t,
		inference.StreamEvent{Kind: inference.StreamToolCall, Index: 0,
			Tool: &inference.ToolCallDelta{ID: "c", Name: "ping"}},
		inference.StreamEvent{Kind: inference.StreamEnd, Finish: inference.FinishToolCalls},
	)
	calls := response.Message.ToolCalls()
	if len(calls) != 1 || !json.Valid(calls[0].Arguments) {
		t.Fatalf("arguments = %q", calls[0].Arguments)
	}
	if string(calls[0].Arguments) != `{}` {
		t.Fatalf("arguments = %q, want {}", calls[0].Arguments)
	}
}

// TestAssemblyReplacesPartAtIndex is the mechanism streamed images rely on: a
// later part at the same index supersedes the earlier one, so the previews never
// survive into the finished answer.
func TestAssemblyReplacesPartAtIndex(t *testing.T) {
	preview := message.TextPart{Text: "preview"}
	final := message.TextPart{Text: "final"}
	response := assemble(t,
		inference.StreamEvent{Kind: inference.StreamPart, Index: 0, Part: preview},
		inference.StreamEvent{Kind: inference.StreamPart, Index: 0, Part: final},
		inference.StreamEvent{Kind: inference.StreamEnd, Finish: inference.FinishStop},
	)
	parts := response.Message.Content.Parts
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want the replaced one only", len(parts))
	}
	if text, ok := parts[0].(message.TextPart); !ok || text.Text != "final" {
		t.Fatalf("part = %+v", parts[0])
	}
}

// TestAssemblyKeepsReasoningSignature pins that reasoning metadata survives
// assembly: providers that expect their reasoning replayed verbatim need it.
func TestAssemblyKeepsReasoningSignature(t *testing.T) {
	response := assemble(t,
		inference.StreamEvent{Kind: inference.StreamReasoning, Index: 0, Text: "think"},
		inference.StreamEvent{Kind: inference.StreamReasoning, Index: 0,
			Signature: "sig", ID: "rid", Source: "openai"},
		inference.StreamEvent{Kind: inference.StreamEnd, Finish: inference.FinishStop},
	)
	parts := response.Message.Content.Parts
	if len(parts) != 1 {
		t.Fatalf("parts = %d", len(parts))
	}
	reasoning, ok := parts[0].(message.ReasoningPart)
	if !ok {
		t.Fatalf("part = %T", parts[0])
	}
	if reasoning.Text != "think" || reasoning.Signature != "sig" ||
		reasoning.ID != "rid" || reasoning.Source != "openai" {
		t.Fatalf("reasoning = %+v", reasoning)
	}
}

// TestAssemblySumsUsage covers providers that report accounting in more than one
// event.
func TestAssemblySumsUsage(t *testing.T) {
	images := int64(1)
	first := usage.Usage{InputTokens: 3, OutputTokens: 0, TotalTokens: 3}
	second := usage.Usage{InputTokens: 0, OutputTokens: 4, TotalTokens: 4, GeneratedImages: &images}
	response := assemble(t,
		inference.StreamEvent{Kind: inference.StreamUsage, Usage: &first},
		inference.StreamEvent{Kind: inference.StreamUsage, Usage: &second},
		inference.StreamEvent{Kind: inference.StreamEnd, Finish: inference.FinishStop},
	)
	if response.Usage.InputTokens != 3 || response.Usage.OutputTokens != 4 ||
		response.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v", response.Usage)
	}
	if response.Usage.GeneratedImages == nil || *response.Usage.GeneratedImages != 1 {
		t.Fatalf("generated images = %v", response.Usage.GeneratedImages)
	}
}

// TestAssemblyKeepsProviderIdentity pins that the response identity from the start
// event reaches the assembled response.
func TestAssemblyKeepsProviderIdentity(t *testing.T) {
	metadata := inference.Metadata{RequestID: "req-1", ResponseID: "resp-1"}
	response := assemble(t,
		inference.StreamEvent{Kind: inference.StreamStart, Metadata: &metadata},
		inference.StreamEvent{Kind: inference.StreamText, Index: 0, Text: "hi"},
		inference.StreamEvent{Kind: inference.StreamEnd, Finish: inference.FinishStop},
	)
	if response.Metadata.ResponseID != "resp-1" || response.Metadata.RequestID != "req-1" {
		t.Fatalf("metadata = %+v", response.Metadata)
	}
}

// audioChunk builds the payload one streamed audio chunk carries: the same media
// type every chunk, the bytes that belong to it.
func audioChunk(t *testing.T, data string) message.Part {
	t.Helper()
	source, err := media.NewAudioBytes([]byte(data), "audio/mpeg")
	if err != nil {
		t.Fatalf("chunk source: %v", err)
	}
	return message.AudioPart{
		Source: source,
		Format: &media.AudioFormat{Encoding: media.AudioEncodingMP3},
	}
}

// TestAssemblyAppendsChunks pins the one merge rule media payloads need: chunks at
// the same index concatenate, and the first one supplies the part template.
func TestAssemblyAppendsChunks(t *testing.T) {
	response := assemble(t,
		inference.StreamEvent{Kind: inference.StreamChunk, Index: 0, Part: audioChunk(t, "abc")},
		inference.StreamEvent{Kind: inference.StreamChunk, Index: 0, Part: audioChunk(t, "def")},
		inference.StreamEvent{Kind: inference.StreamChunk, Index: 0, Part: audioChunk(t, "ghi")},
		inference.StreamEvent{Kind: inference.StreamEnd, Finish: inference.FinishStop},
	)
	parts := response.Message.Content.Parts
	if len(parts) != 1 {
		t.Fatalf("parts = %d", len(parts))
	}
	audio, ok := parts[0].(message.AudioPart)
	if !ok {
		t.Fatalf("part = %T", parts[0])
	}
	if got := string(audio.Source.Bytes()); got != "abcdefghi" {
		t.Fatalf("bytes = %q", got)
	}
	if audio.Source.MediaType() != "audio/mpeg" {
		t.Fatalf("media type = %q", audio.Source.MediaType())
	}
	if audio.Format == nil || audio.Format.Encoding != media.AudioEncodingMP3 {
		t.Fatalf("template format lost: %+v", audio.Format)
	}
}

// TestAssemblyPartSupersedesChunks pins the precedence: a full part at an index
// is authoritative over whatever chunks arrived for it.
func TestAssemblyPartSupersedesChunks(t *testing.T) {
	final := message.TextPart{Text: "authoritative"}
	response := assemble(t,
		inference.StreamEvent{Kind: inference.StreamChunk, Index: 0, Part: audioChunk(t, "abc")},
		inference.StreamEvent{Kind: inference.StreamPart, Index: 0, Part: final},
		inference.StreamEvent{Kind: inference.StreamEnd, Finish: inference.FinishStop},
	)
	parts := response.Message.Content.Parts
	if len(parts) != 1 {
		t.Fatalf("parts = %d", len(parts))
	}
	if text, ok := parts[0].(message.TextPart); !ok || text.Text != "authoritative" {
		t.Fatalf("part = %+v, want the delivered one", parts[0])
	}
}

// TestAssemblyKeepsChunksOfDifferentIndexesApart is the multi-track case: two
// chunked parts must not interleave into one.
func TestAssemblyKeepsChunksOfDifferentIndexesApart(t *testing.T) {
	response := assemble(t,
		inference.StreamEvent{Kind: inference.StreamChunk, Index: 0, Part: audioChunk(t, "aa")},
		inference.StreamEvent{Kind: inference.StreamChunk, Index: 1, Part: audioChunk(t, "bb")},
		inference.StreamEvent{Kind: inference.StreamChunk, Index: 0, Part: audioChunk(t, "cc")},
		inference.StreamEvent{Kind: inference.StreamEnd, Finish: inference.FinishStop},
	)
	parts := response.Message.Content.Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %d", len(parts))
	}
	first := parts[0].(message.AudioPart)
	second := parts[1].(message.AudioPart)
	if string(first.Source.Bytes()) != "aacc" || string(second.Source.Bytes()) != "bb" {
		t.Fatalf("chunks interleaved: %q / %q",
			first.Source.Bytes(), second.Source.Bytes())
	}
}
