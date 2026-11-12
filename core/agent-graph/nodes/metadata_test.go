package nodes_test

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph/nodes"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// announcingProvider stands in for a driver that names the model it resolved
// before it starts emitting: the opening event carries it, and the response
// repeats it.
type announcingProvider struct {
	metadata inference.Metadata
	text     string
}

func (p *announcingProvider) Generate(ctx context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	inner, err := p.GenerateStream(ctx, request)
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	for {
		if _, err := inner.Next(ctx); err != nil {
			if err == io.EOF {
				break
			}
			return inference.GenerateResponse{}, err
		}
	}
	return inner.Result()
}

func (p *announcingProvider) GenerateStream(context.Context, inference.GenerateRequest) (inference.Stream, error) {
	metadata := p.metadata
	return &announcingStream{
		events: []inference.StreamEvent{
			{Kind: inference.StreamStart, Metadata: &metadata},
			{Kind: inference.StreamText, Text: p.text},
		},
		response: inference.GenerateResponse{
			Message:  textResponse(p.text).Message,
			Metadata: metadata,
		},
	}, nil
}

type announcingStream struct {
	events   []inference.StreamEvent
	response inference.GenerateResponse
}

func (s *announcingStream) Next(context.Context) (inference.StreamEvent, error) {
	if len(s.events) == 0 {
		return inference.StreamEvent{}, io.EOF
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}

func (s *announcingStream) Result() (inference.GenerateResponse, error) { return s.response, nil }
func (*announcingStream) Close() error                                  { return nil }

// resolved is what the provider says answered, which is not what the request
// asked for: an alias may route to another model.
func resolved() inference.Metadata {
	return inference.Metadata{
		Model:      model.ModelID{Provider: "stub", Name: "resolved-model"},
		RequestID:  "req-1",
		ResponseID: "resp-1",
	}
}

// The round opens exactly once, before any delta, and names the model that
// answered rather than the one the request named.
func TestModelEmitsStreamStartOnce(t *testing.T) {
	provider := &announcingProvider{metadata: resolved(), text: "ok"}
	kind := nodes.Model(testRuntime(t, provider))
	invoke := modelInvoke(telemetry.TraceNode, false)
	invoke.Step = 2

	stream, err := kind.Invoke(context.Background(), invoke)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	_, events := drainNode(t, stream)

	var starts []agent.StreamStart
	for _, event := range events {
		if typed, ok := event.(agent.StreamStart); ok {
			starts = append(starts, typed)
		}
	}
	if len(starts) != 1 {
		t.Fatalf("stream-start events = %d, want one per round", len(starts))
	}
	if starts[0].Round != invoke.Step+1 {
		t.Errorf("round = %d, want the step's ordinal %d", starts[0].Round, invoke.Step+1)
	}
	if starts[0].Model != "resolved-model" {
		t.Errorf("model = %q, want the resolved one", starts[0].Model)
	}
	// The announcement leads: the engine resets its streamed text on it.
	if _, ok := events[0].(agent.StreamStart); !ok {
		t.Errorf("first event = %T, want the round's start", events[0])
	}
}

// The record carries the answer's identity even when the body is not kept: a
// dataset built from records alone still knows which endpoint answered.
func TestModelRecordCarriesMetadata(t *testing.T) {
	provider := &announcingProvider{metadata: resolved(), text: "ok"}
	kind := nodes.Model(testRuntime(t, provider))

	stream, err := kind.Invoke(context.Background(), modelInvoke(telemetry.TraceNode, false))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, _ := drainNode(t, stream)
	calls := callRecords(t, result)
	if len(calls) != 1 {
		t.Fatalf("%s records = %d, want 1", recordLLMCall, len(calls))
	}
	call := calls[0]
	if len(call.Response) != 0 {
		t.Error("bodies is off, but the response was stored")
	}
	if call.Metadata.Model.Name != "resolved-model" {
		t.Errorf("metadata model = %q, want the resolved one", call.Metadata.Model.Name)
	}
	if call.Metadata.RequestID != "req-1" || call.Metadata.ResponseID != "resp-1" {
		t.Errorf("provider ids = %q/%q, want req-1/resp-1",
			call.Metadata.RequestID, call.Metadata.ResponseID)
	}
	// The request's own model stays where it was: the two facts are different.
	if call.Model.ID.Name == "resolved-model" {
		t.Error("the request model was overwritten with the resolved one")
	}
}

// A replayed round reports the identity the original one reported, so a log
// cannot tell the two apart by what the answer claims to be.
func TestModelReplayKeepsMetadata(t *testing.T) {
	kind := nodes.Model(testRuntime(t, &stubProvider{}))
	response, err := json.Marshal(textResponse("from record").Message)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	invoke := modelInvoke(telemetry.TraceNode, true)
	invoke.Replay = true
	invoke.Lookup = func(string) (agent.LLMCallRecord, bool) {
		return agent.LLMCallRecord{Response: response, Metadata: resolved()}, true
	}
	stream, err := kind.Invoke(context.Background(), invoke)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	_, events := drainNode(t, stream)

	var starts []agent.StreamStart
	for _, event := range events {
		if typed, ok := event.(agent.StreamStart); ok {
			starts = append(starts, typed)
		}
	}
	if len(starts) != 1 {
		t.Fatalf("stream-start events = %d, want one", len(starts))
	}
	if starts[0].Model != "resolved-model" {
		t.Errorf("a replayed round named %q, want the recorded model", starts[0].Model)
	}
}
