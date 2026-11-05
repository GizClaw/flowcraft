package openai

import (
	"context"
	"fmt"

	"github.com/openai/openai-go/v3/responses"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
)

// responsesStream adapts the SDK's Responses event stream to the protocol's
// wire-level stream.

// responsesOpen starts the streaming call.
func (d *driver) responsesOpen(
	ctx context.Context,
	request inference.GenerateRequest,
	params responses.ResponseNewParams,
) (protocol.RawStream[responses.ResponseStreamEventUnion], error) {
	extra, err := extensionJSONSet(request.Extensions)
	if err != nil {
		return nil, err
	}
	opts, err := bodyOptions(params, extra)
	if err != nil {
		return nil, err
	}
	stream := d.client.Responses.NewStreaming(ctx, params, opts...)
	return newSDKStream(stream.Next, stream.Current, stream.Err, stream.Close), nil
}

// responsesStreamDecoder builds the per-stream decoder. Every event carries the
// response it belongs to, so identity is emitted once, on the first event that
// knows it.
func responsesStreamDecoder(_ inference.GenerateRequest) protocol.StreamDecoder[responses.ResponseStreamEventUnion] {
	started := false
	return func(_ context.Context, event responses.ResponseStreamEventUnion) ([]inference.StreamEvent, error) {
		return responsesStreamEvents(event, &started)
	}
}

// responsesStreamEvents maps one Responses event onto canonical events. The
// surface names every event explicitly, so the mapping is a switch over the
// event type rather than an inspection of a delta object.
func responsesStreamEvents(
	event responses.ResponseStreamEventUnion,
	started *bool,
) ([]inference.StreamEvent, error) {
	events, err := responsesEventPayload(event)
	if err != nil {
		return nil, err
	}
	if !*started && event.Response.ID != "" {
		*started = true
		metadata := inference.Metadata{
			Model:      model.ModelID{Provider: ProviderID, Name: string(event.Response.Model)},
			ResponseID: event.Response.ID,
		}
		events = append([]inference.StreamEvent{{
			Kind:     inference.StreamStart,
			Metadata: &metadata,
		}}, events...)
	}
	return events, nil
}

func responsesEventPayload(event responses.ResponseStreamEventUnion) ([]inference.StreamEvent, error) {
	switch string(event.Type) {
	case "response.output_text.delta":
		return []inference.StreamEvent{{
			Kind:  inference.StreamText,
			Index: chatTextIndex,
			Text:  event.Delta,
		}}, nil

	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		return []inference.StreamEvent{{
			Kind:  inference.StreamReasoning,
			Index: chatReasoningIndex,
			Text:  event.Delta,
		}}, nil

	case "response.output_item.added":
		if event.Item.Type != "function_call" {
			return nil, nil
		}
		return []inference.StreamEvent{{
			Kind:  inference.StreamToolCall,
			Index: chatToolIndexBase + int(event.OutputIndex),
			Tool: &inference.ToolCallDelta{
				ID:   event.Item.CallID,
				Name: event.Item.Name,
			},
		}}, nil

	case "response.function_call_arguments.delta":
		return []inference.StreamEvent{{
			Kind:  inference.StreamToolCall,
			Index: chatToolIndexBase + int(event.OutputIndex),
			Tool: &inference.ToolCallDelta{
				Arguments: event.Delta,
			},
		}}, nil

	case "response.completed", "response.incomplete":
		response := event.Response
		accounting := responsesUsage(&response)
		finish, err := responsesFinishReason(string(response.Status), countFunctionCalls(response.Output))
		if err != nil {
			return nil, err
		}
		events := []inference.StreamEvent{{
			Kind:  inference.StreamUsage,
			Usage: &accounting,
		}}
		return append(events, inference.StreamEvent{
			Kind:   inference.StreamEnd,
			Finish: finish,
		}), nil

	case "response.failed":
		return nil, inference.NewError(
			inference.ProviderFailure,
			inference.FieldStream,
			fmt.Errorf("openai: response failed: %s", event.Response.Error.Message),
		)
	}

	// Everything else is progress reporting the contract does not surface:
	// content-part bookkeeping, search calls, refusal deltas.
	return nil, nil
}

func countFunctionCalls(items []responses.ResponseOutputItemUnion) int {
	count := 0
	for _, item := range items {
		if item.Type == "function_call" {
			count++
		}
	}
	return count
}
