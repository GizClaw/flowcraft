package anthropic

import (
	"context"
	"errors"
	"io"

	anthropicgo "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
)

// messageStream adapts the SDK's SSE reader to the protocol's wire stream.
type messageStream struct {
	stream *ssestream.Stream[anthropicgo.MessageStreamEventUnion]
}

func (s *messageStream) Next(ctx context.Context) (anthropicgo.MessageStreamEventUnion, error) {
	if err := ctx.Err(); err != nil {
		return anthropicgo.MessageStreamEventUnion{}, inference.NewError(
			inference.OperationInterrupted, "", err)
	}
	if !s.stream.Next() {
		if err := s.stream.Err(); err != nil {
			return anthropicgo.MessageStreamEventUnion{}, classifyError(err)
		}
		return anthropicgo.MessageStreamEventUnion{}, io.EOF
	}
	return s.stream.Current(), nil
}

func (s *messageStream) Close() error {
	if s.stream == nil {
		return nil
	}
	return s.stream.Close()
}

func (d *driver) open(
	ctx context.Context,
	_ inference.GenerateRequest,
	params anthropicgo.MessageNewParams,
) (protocol.RawStream[anthropicgo.MessageStreamEventUnion], error) {
	stream := d.client.Messages.NewStreaming(ctx, params)
	if err := stream.Err(); err != nil {
		return nil, classifyError(err)
	}
	return &messageStream{stream: stream}, nil
}

type streamPart struct {
	index     int
	reasoning bool
}

// newStreamDecoder builds the per-stream decoder: it assigns canonical part
// indices to content blocks and folds the API's block events into deltas.
func (d *driver) newStreamDecoder(
	request inference.GenerateRequest,
) protocol.StreamDecoder[anthropicgo.MessageStreamEventUnion] {
	var (
		started    bool
		responseID string
		nextPart   int
		parts      = make(map[int64]streamPart)
		usage      anthropicgo.Usage
		ended      bool
		scope      = d.reasoningScope(request)
	)
	register := func(index int64, reasoning bool) streamPart {
		part, ok := parts[index]
		if !ok {
			part = streamPart{index: nextPart, reasoning: reasoning}
			nextPart++
			parts[index] = part
		}
		return part
	}
	return func(_ context.Context, event anthropicgo.MessageStreamEventUnion) ([]inference.StreamEvent, error) {
		switch event.Type {
		case "message_start":
			responseID = event.Message.ID
			usage = event.Message.Usage
			if started {
				return nil, nil
			}
			started = true
			metadata := inference.Metadata{
				Model:      model.ModelID{Provider: ProviderID, Name: request.Model.ID.Name},
				ResponseID: responseID,
			}
			return []inference.StreamEvent{{
				Kind:     inference.StreamStart,
				Metadata: &metadata,
			}}, nil

		case "content_block_start":
			block := event.ContentBlock
			switch block.Type {
			case "thinking":
				register(event.Index, true)
				return nil, nil
			case "redacted_thinking":
				part := register(event.Index, true)
				return []inference.StreamEvent{{
					Kind:      inference.StreamReasoning,
					Index:     part.index,
					Signature: block.Data,
					Source:    scope,
				}}, nil
			case "tool_use":
				part := register(event.Index, false)
				return []inference.StreamEvent{{
					Kind:  inference.StreamToolCall,
					Index: part.index,
					Tool: &inference.ToolCallDelta{
						ID:   block.ID,
						Name: block.Name,
					},
				}}, nil
			default:
				register(event.Index, false)
				return nil, nil
			}

		case "content_block_delta":
			part, ok := parts[event.Index]
			if !ok {
				return nil, nil
			}
			if part.reasoning {
				switch event.Delta.Type {
				case "thinking_delta":
					if event.Delta.Thinking == "" {
						return nil, nil
					}
					return []inference.StreamEvent{{
						Kind:   inference.StreamReasoning,
						Index:  part.index,
						Text:   event.Delta.Thinking,
						Source: scope,
					}}, nil
				case "signature_delta":
					if event.Delta.Signature == "" {
						return nil, nil
					}
					return []inference.StreamEvent{{
						Kind:      inference.StreamReasoning,
						Index:     part.index,
						Signature: event.Delta.Signature,
						Source:    scope,
					}}, nil
				default:
					return nil, nil
				}
			}
			switch event.Delta.Type {
			case "text_delta":
				if event.Delta.Text == "" {
					return nil, nil
				}
				return []inference.StreamEvent{{
					Kind:  inference.StreamText,
					Index: part.index,
					Text:  event.Delta.Text,
				}}, nil
			case "input_json_delta":
				if event.Delta.PartialJSON == "" {
					return nil, nil
				}
				return []inference.StreamEvent{{
					Kind:  inference.StreamToolCall,
					Index: part.index,
					Tool:  &inference.ToolCallDelta{Arguments: event.Delta.PartialJSON},
				}}, nil
			default:
				return nil, nil
			}

		case "message_delta":
			if ended {
				return nil, inference.NewError(
					inference.InvalidProviderResponse, inference.FieldStream,
					errors.New("anthropic: duplicate terminal event"))
			}
			ended = true
			usage.OutputTokens = event.Usage.OutputTokens
			usage.OutputTokensDetails = event.Usage.OutputTokensDetails
			usage.ServerToolUse = event.Usage.ServerToolUse
			finish, err := finishReason(event.Delta.StopReason, false)
			if err != nil {
				return nil, err
			}
			accounting := usageOf(usage)
			return []inference.StreamEvent{
				{Kind: inference.StreamUsage, Usage: &accounting},
				{Kind: inference.StreamEnd, Finish: finish},
			}, nil

		case "message_stop":
			if ended {
				return nil, nil
			}
			ended = true
			accounting := usageOf(usage)
			return []inference.StreamEvent{
				{Kind: inference.StreamUsage, Usage: &accounting},
				{Kind: inference.StreamEnd, Finish: inference.FinishStop},
			}, nil
		default:
			return nil, nil
		}
	}
}
