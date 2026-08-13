package anthropic

import (
	"context"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"

	anthropicgo "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
)

// messagesStream adapts the SDK SSE reader to ProviderStream[streamRaw]. It
// is the stateful stage of the streaming pipeline: it assigns canonical part
// indices to content blocks and folds the API's per-block events into
// deltas, so the decoder function stays pure.
type messagesStream struct {
	stream *ssestream.Stream[anthropicgo.MessageStreamEventUnion]

	parts    map[int64]*streamPart // content block index → canonical part
	nextPart int
	finished bool
	sawTools bool
	usage    rawUsage // accumulates message_start input + message_delta output
	id       string   // message id from message_start
}

type streamPart struct {
	index     int
	tool      bool
	reasoning bool // thinking and redacted blocks emit reasoning deltas
}

func transportGenerateStream(
	client anthropicgo.Client,
) inference.Transport[generateWire, inference.ProviderStream[streamRaw]] {
	return func(
		ctx context.Context,
		wire generateWire,
	) (inference.ProviderStream[streamRaw], error) {
		stream := client.Messages.NewStreaming(ctx, wireToParams(wire))
		if err := stream.Err(); err != nil {
			return nil, classifyError(err)
		}
		return &messagesStream{
			stream: stream,
			parts:  make(map[int64]*streamPart),
		}, nil
	}
}

func (s *messagesStream) Close() error {
	if s.stream == nil {
		return nil
	}
	return classifyError(s.stream.Close())
}

func (s *messagesStream) Next(ctx context.Context) (streamRaw, error) {
	if err := ctx.Err(); err != nil {
		return streamRaw{}, errdefs.FromContext(err)
	}
	for {
		if !s.stream.Next() {
			if err := s.stream.Err(); err != nil {
				return streamRaw{}, classifyError(err)
			}
			return streamRaw{}, io.EOF
		}
		raw, keep, err := s.apply(s.stream.Current())
		if err != nil {
			return streamRaw{}, err
		}
		if keep {
			return raw, nil
		}
	}
}

// apply folds one stream event into a streamRaw. keep=false means the event
// was bookkeeping-only (part registration, thinking blocks, pings) and the
// loop should read on.
func (s *messagesStream) apply(
	event anthropicgo.MessageStreamEventUnion,
) (streamRaw, bool, error) {
	switch event.Type {
	case "message_start":
		s.id = event.Message.ID
		s.usage = usageFromSDK(event.Message.Usage)
		return streamRaw{}, false, nil
	case "content_block_start":
		return s.applyBlockStart(event.Index, event.ContentBlock)
	case "content_block_delta":
		return s.applyBlockDelta(event)
	case "message_delta":
		s.usage.outputTokens = event.Usage.OutputTokens
		s.usage.thinkingTokens = event.Usage.OutputTokensDetails.ThinkingTokens
		s.usage.webSearchRequests = event.Usage.ServerToolUse.WebSearchRequests
		s.usage.webFetchRequests = event.Usage.ServerToolUse.WebFetchRequests
		return s.finishEvent(stopReasonFinish(event.Delta.StopReason, s.sawTools))
	}
	// content_block_stop, message_stop, ping: lifecycle bookkeeping. The
	// terminal event fires on message_delta, which carries the stop reason;
	// message_stop itself adds nothing.
	return streamRaw{}, false, nil
}

func (s *messagesStream) applyBlockStart(
	index int64,
	block anthropicgo.ContentBlockStartEventContentBlockUnion,
) (streamRaw, bool, error) {
	switch block.Type {
	case "tool_use":
		part := s.registerPart(index, true)
		return streamRaw{
			kind: streamRawToolFragment,
			part: part.index,
			tool: streamRawTool{id: block.ID, name: block.Name},
		}, true, nil
	case "thinking":
		part := s.registerPart(index, false)
		part.reasoning = true
		return streamRaw{}, false, nil
	case "redacted_thinking":
		part := s.registerPart(index, false)
		part.reasoning = true
		// Redacted blocks arrive whole at block start: one terminal delta
		// carries the opaque data in the signature slot.
		return streamRaw{
			kind:      streamRawReasoning,
			part:      part.index,
			signature: block.Data,
		}, true, nil
	default: // text and any server-side blocks
		s.registerPart(index, false)
		return streamRaw{}, false, nil
	}
}

func (s *messagesStream) applyBlockDelta(
	event anthropicgo.MessageStreamEventUnion,
) (streamRaw, bool, error) {
	part, ok := s.parts[event.Index]
	if !ok {
		return streamRaw{}, false, nil
	}
	if part.reasoning {
		return reasoningDelta(part.index, event.Delta)
	}
	switch event.Delta.Type {
	case "text_delta":
		if event.Delta.Text == "" {
			return streamRaw{}, false, nil
		}
		return streamRaw{
			kind: streamRawText,
			part: part.index,
			text: event.Delta.Text,
		}, true, nil
	case "input_json_delta":
		if event.Delta.PartialJSON == "" {
			return streamRaw{}, false, nil
		}
		return streamRaw{
			kind: streamRawToolFragment,
			part: part.index,
			tool: streamRawTool{argsFragment: event.Delta.PartialJSON},
		}, true, nil
	}
	// citations_delta and anything else: no canonical surface.
	return streamRaw{}, false, nil
}

// reasoningDelta folds thinking block deltas. The signature arrives once, on
// the terminal signature_delta, and the canonical accumulator keeps it.
func reasoningDelta(
	part int,
	delta anthropicgo.MessageStreamEventUnionDelta,
) (streamRaw, bool, error) {
	switch delta.Type {
	case "thinking_delta":
		if delta.Thinking == "" {
			return streamRaw{}, false, nil
		}
		return streamRaw{
			kind: streamRawReasoning,
			part: part,
			text: delta.Thinking,
		}, true, nil
	case "signature_delta":
		if delta.Signature == "" {
			return streamRaw{}, false, nil
		}
		return streamRaw{
			kind:      streamRawReasoning,
			part:      part,
			signature: delta.Signature,
		}, true, nil
	}
	return streamRaw{}, false, nil
}

// registerPart assigns a stable canonical part index per content block.
func (s *messagesStream) registerPart(index int64, tool bool) *streamPart {
	part, ok := s.parts[index]
	if !ok {
		part = &streamPart{index: s.nextPart, tool: tool}
		s.nextPart++
		s.parts[index] = part
	}
	if tool {
		s.sawTools = true
	}
	return part
}

// finishEvent renders the single terminal event. A duplicate terminal event
// would violate the runtime's single-finish invariant, so it is an error.
func (s *messagesStream) finishEvent(
	finish inference.FinishReason,
) (streamRaw, bool, error) {
	if s.finished {
		return streamRaw{}, false, errdefs.Internalf(
			"anthropic: stream emitted a duplicate terminal event",
		)
	}
	s.finished = true
	usage := s.usage
	return streamRaw{
		kind:       streamRawFinish,
		usage:      &usage,
		finish:     finish,
		responseID: s.id,
	}, true, nil
}

// decodeGenerateStream is pure: streamRaw already carries canonical part
// indices assigned by the stateful transport.
func decodeGenerateStream(
	_ context.Context,
	raw streamRaw,
) (inference.GenerateStreamEvent, error) {
	switch raw.kind {
	case streamRawText:
		return inference.GenerateStreamEvent{
			PartIndex: raw.part,
			Delta:     inference.TextPartDelta{Text: raw.text},
		}, nil
	case streamRawToolFragment:
		return inference.GenerateStreamEvent{
			PartIndex: raw.part,
			Delta: inference.ToolCallDelta{
				ID:                raw.tool.id,
				Name:              raw.tool.name,
				ArgumentsFragment: raw.tool.argsFragment,
			},
		}, nil
	case streamRawReasoning:
		return inference.GenerateStreamEvent{
			PartIndex: raw.part,
			Delta: inference.ReasoningDelta{
				Text:      raw.text,
				Signature: raw.signature,
			},
		}, nil
	case streamRawFinish:
		event := inference.GenerateStreamEvent{
			FinishReason: raw.finish,
			ResponseID:   raw.responseID,
		}
		if raw.usage != nil {
			usage := rawUsageCanonical(*raw.usage)
			event.Usage = &usage
		}
		return event, nil
	}
	return inference.GenerateStreamEvent{}, fmt.Errorf(
		"anthropic: unknown stream raw kind %d",
		raw.kind,
	)
}
