package inference

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

// streamEventList is a scripted ProviderStream over fully decoded events;
// the decode stage is the identity function, so failures are produced by
// the runtime accumulator rather than a driver.
type streamEventList struct {
	events []GenerateStreamEvent
	next   int
}

func (s *streamEventList) Next(context.Context) (GenerateStreamEvent, error) {
	if s.next < len(s.events) {
		event := s.events[s.next]
		s.next++
		return event, nil
	}
	return GenerateStreamEvent{}, io.EOF
}

func (*streamEventList) Close() error { return nil }

// metaStreamList carries provider identifiers through the optional
// ProviderStreamMetadata capability, as driver transports do after
// capturing x-request-id at stream open.
type metaStreamList struct {
	*streamEventList
	requestID  string
	responseID string
}

func (s *metaStreamList) RequestID() string  { return s.requestID }
func (s *metaStreamList) ResponseID() string { return s.responseID }

func identityGenerateDecoder(
	_ context.Context,
	event GenerateStreamEvent,
) (GenerateStreamEvent, error) {
	return event, nil
}

func detailTestStream(events []GenerateStreamEvent, request GenerateRequest) *decodedGenerateStream[GenerateStreamEvent] {
	return &decodedGenerateStream[GenerateStreamEvent]{
		raw:       &streamEventList{events: events},
		decode:    identityGenerateDecoder,
		model:     ModelRef{ID: ModelID{Provider: "fake", Name: "model-1"}},
		request:   request,
		parts:     make(map[int]*generatePartAccumulator),
		startedAt: time.Now(),
	}
}

func detailTestStreamWithMeta(
	events []GenerateStreamEvent,
	request GenerateRequest,
	requestID, responseID string,
) *decodedGenerateStream[GenerateStreamEvent] {
	stream := detailTestStream(events, request)
	stream.raw = &metaStreamList{
		streamEventList: &streamEventList{events: events},
		requestID:       requestID,
		responseID:      responseID,
	}
	stream.meta = stream.raw.(ProviderStreamMetadata)
	return stream
}

func toolStreamRequest(t *testing.T) GenerateRequest {
	t.Helper()
	return GenerateRequest{
		Input: GenerateInput{
			Role: InputRoleUser,
			Content: InputContent{
				Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
				Intent: Intent{Text: &TextIntent{
					Tools: []message.ToolDefinition{
						message.DefineSchema("known", "a known tool").Build(),
					},
				}},
			},
		},
	}
}

func nextStreamError(t *testing.T, stream GenerateStream) *Error {
	t.Helper()
	for {
		_, err := stream.Next(context.Background())
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			t.Fatal("stream ended cleanly, want failure")
		}
		var out *Error
		if !errors.As(err, &out) {
			t.Fatalf("Next error = %v, want *inference.Error", err)
		}
		return out
	}
}

func TestGenerateStreamMissingFinishDetail(t *testing.T) {
	stream := detailTestStream(nil, GenerateRequest{})
	err := nextStreamError(t, stream)
	if err.Kind != ProviderTruncated {
		t.Fatalf("kind = %q, want %q", err.Kind, ProviderTruncated)
	}
	if err.Detail != "stream.finish.missing" {
		t.Fatalf("Detail = %q, want stream.finish.missing", err.Detail)
	}
	if got := err.Error(); got != "provider_truncated during generate: stream.finish.missing" {
		t.Fatalf("Error() = %q", got)
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("provider_truncated must classify as not available, got %v", err)
	}
}

func TestGenerateStreamTruncationCarriesProviderRequestID(t *testing.T) {
	stream := detailTestStreamWithMeta(nil, GenerateRequest{}, "req-openai-1", "resp-1")
	err := nextStreamError(t, stream)
	if err.Kind != ProviderTruncated {
		t.Fatalf("kind = %q, want %q", err.Kind, ProviderTruncated)
	}
	if err.RequestID != "req-openai-1" {
		t.Fatalf("Error.RequestID = %q, want req-openai-1", err.RequestID)
	}
	if got, ok := errdefs.RequestID(err); !ok || got != "req-openai-1" {
		t.Fatalf("errdefs.RequestID(err) = %q/%v, want req-openai-1/true", got, ok)
	}
}

func TestGenerateStreamTruncationFallsBackToResponseID(t *testing.T) {
	stream := detailTestStreamWithMeta(nil, GenerateRequest{}, "", "chatcmpl-1")
	err := nextStreamError(t, stream)
	if err.RequestID != "chatcmpl-1" {
		t.Fatalf("Error.RequestID = %q, want chatcmpl-1 fallback", err.RequestID)
	}
}

func TestGenerateStreamToolCallStructureDetail(t *testing.T) {
	events := []GenerateStreamEvent{
		{PartIndex: 0, Delta: ToolCallDelta{
			ID:                "call-1",
			Name:              "known",
			ArgumentsFragment: `{"query": "x`,
		}},
		{FinishReason: FinishToolCalls},
	}
	stream := detailTestStream(events, toolStreamRequest(t))
	err := nextStreamError(t, stream)
	if err.Kind != InvalidProviderResponse {
		t.Fatalf("kind = %q, want %q", err.Kind, InvalidProviderResponse)
	}
	if err.Detail != "stream.finish.tool_call" {
		t.Fatalf("Detail = %q, want stream.finish.tool_call", err.Detail)
	}
}

func TestGenerateStreamToolCallConflictDetail(t *testing.T) {
	events := []GenerateStreamEvent{
		{PartIndex: 0, Delta: ToolCallDelta{ID: "call-1", Name: "known"}},
		{PartIndex: 0, Delta: ToolCallDelta{ID: "call-2", Name: "known"}},
	}
	stream := detailTestStream(events, toolStreamRequest(t))
	err := nextStreamError(t, stream)
	if err.Detail != "stream.accumulate.tool_call" {
		t.Fatalf("Detail = %q, want stream.accumulate.tool_call", err.Detail)
	}
}

func TestGenerateStreamToolCallFinishMismatchDetail(t *testing.T) {
	events := []GenerateStreamEvent{{FinishReason: FinishToolCalls}}
	stream := detailTestStream(events, toolStreamRequest(t))
	err := nextStreamError(t, stream)
	if err.Detail != "stream.finish.mismatch" {
		t.Fatalf("Detail = %q, want stream.finish.mismatch", err.Detail)
	}
}

func TestGenerateStreamUndefinedToolDetail(t *testing.T) {
	events := []GenerateStreamEvent{
		{PartIndex: 0, Delta: ToolCallDelta{
			ID:                "call-1",
			Name:              "ghost",
			ArgumentsFragment: `{}`,
		}},
		{FinishReason: FinishToolCalls},
	}
	stream := detailTestStream(events, toolStreamRequest(t))
	err := nextStreamError(t, stream)
	if err.Kind != UndefinedTool {
		t.Fatalf("kind = %q, want %q", err.Kind, UndefinedTool)
	}
	if err.Detail != "stream.finish.undefined_tool" {
		t.Fatalf("Detail = %q, want stream.finish.undefined_tool", err.Detail)
	}
}

func TestErrorDetailFormatting(t *testing.T) {
	err := NewError(InvalidProviderResponse, OperationGenerate, "", errors.New("boom"))
	if err.Detail != "" {
		t.Fatalf("NewError must leave Detail empty, got %q", err.Detail)
	}
	if got := err.Error(); got != "invalid_provider_response during generate" {
		t.Fatalf("Error() without Detail = %q", got)
	}
	err.Detail = "stream.decode"
	if got := err.Error(); got != "invalid_provider_response during generate: stream.decode" {
		t.Fatalf("Error() with Detail = %q", got)
	}
}

func textStreamRequest(t *testing.T) GenerateRequest {
	t.Helper()
	return GenerateRequest{
		Input: GenerateInput{
			Role: InputRoleUser,
			Content: InputContent{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "hi"},
				}},
				Intent: Intent{Text: &TextIntent{}},
			},
		},
	}
}

func drainGenerateStreamResult(t *testing.T, stream GenerateStream) GenerateResponse {
	t.Helper()
	for {
		_, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
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
	return response
}

func TestGenerateStreamFinishSynthesizedFlagPropagates(t *testing.T) {
	stream := detailTestStream([]GenerateStreamEvent{
		{PartIndex: 0, Delta: TextPartDelta{Text: "hi"}},
		{FinishReason: FinishCompleted, FinishSynthesized: true},
	}, textStreamRequest(t))
	response := drainGenerateStreamResult(t, stream)
	if !response.FinishSynthesized {
		t.Fatal("synthesized finish must surface on the response")
	}
	if response.FinishReason != FinishCompleted {
		t.Fatalf("finish reason = %q, want completed", response.FinishReason)
	}
}

func TestGenerateStreamExplicitFinishFlagFalse(t *testing.T) {
	stream := detailTestStream([]GenerateStreamEvent{
		{PartIndex: 0, Delta: TextPartDelta{Text: "hi"}},
		{FinishReason: FinishCompleted},
	}, textStreamRequest(t))
	response := drainGenerateStreamResult(t, stream)
	if response.FinishSynthesized {
		t.Fatal("provider-emitted finish must not be marked synthesized")
	}
}

func TestGenerateStreamFinishSynthesizedRequiresFinishReason(t *testing.T) {
	stream := detailTestStream([]GenerateStreamEvent{
		{PartIndex: 0, Delta: TextPartDelta{Text: "hi"}},
		{FinishSynthesized: true},
	}, textStreamRequest(t))
	err := nextStreamError(t, stream)
	if err.Kind != InvalidProviderResponse {
		t.Fatalf("kind = %q, want %q", err.Kind, InvalidProviderResponse)
	}
	if err.Detail != "stream.accumulate" {
		t.Fatalf("Detail = %q, want stream.accumulate", err.Detail)
	}
}
