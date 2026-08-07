package deepseek

import (
	"context"
	"io"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"

	openaigo "github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/ssestream"
)

// chatStream adapts the SDK's SSE stream to the provider stream contract.
// It is the stateful stage: it assigns canonical part indices as deltas
// arrive (reasoning precedes text on this surface), folds chunk fields
// into delta events, and holds the finish event back until the stream ends
// so usage rides along — DeepSeek delivers usage in its own chunk after
// the finish_reason chunk.
type chatStream struct {
	stream *ssestream.Stream[openaigo.ChatCompletionChunk]

	pending []streamRaw

	reasoningPart int
	textPart      int
	toolParts     map[int64]int
	nextPart      int

	finish    inference.FinishReason
	finishErr error
	usage     *rawUsage
	sawTools  bool
	ended     bool
	id        string // chat completion id from the stream chunks
}

// transportGenerateStream opens the streaming request and returns the
// stateful provider stream.
func transportGenerateStream(client openaigo.Client) inference.Transport[generateWire, inference.ProviderStream[streamRaw]] {
	return func(ctx context.Context, wire generateWire) (inference.ProviderStream[streamRaw], error) {
		params, overrides := wireToParams(wire)
		stream := client.Chat.Completions.NewStreaming(ctx, params, overrides...)
		if stream == nil {
			return nil, errdefs.NotAvailablef("deepseek: nil stream handle (provider misbehaviour)")
		}
		if err := stream.Err(); err != nil {
			return nil, classifyError(err)
		}
		return &chatStream{
			stream:        stream,
			reasoningPart: -1,
			textPart:      -1,
			toolParts:     make(map[int64]int),
		}, nil
	}
}

func (s *chatStream) Close() error {
	return s.stream.Close()
}

func (s *chatStream) Next(ctx context.Context) (streamRaw, error) {
	if err := ctx.Err(); err != nil {
		return streamRaw{}, errdefs.FromContext(err)
	}
	for len(s.pending) == 0 {
		if s.ended {
			if s.finishErr != nil {
				err := s.finishErr
				s.finishErr = nil
				return streamRaw{}, err
			}
			return streamRaw{}, io.EOF
		}
		if !s.stream.Next() {
			if err := s.stream.Err(); err != nil {
				return streamRaw{}, classifyError(err)
			}
			s.end()
			continue
		}
		s.apply(s.stream.Current())
		if err := ctx.Err(); err != nil {
			return streamRaw{}, errdefs.FromContext(err)
		}
	}
	event := s.pending[0]
	s.pending = s.pending[1:]
	return event, nil
}

// apply folds one chunk into zero or more delta events. Finish reasons and
// usage are recorded, not emitted: the finish event ships at stream end so
// it carries both.
func (s *chatStream) apply(chunk openaigo.ChatCompletionChunk) {
	if chunk.ID != "" {
		s.id = chunk.ID
	}
	if chunk.Usage.JSON.PromptTokens.Valid() || chunk.Usage.JSON.CompletionTokens.Valid() {
		usage := usageToRaw(chunk.Usage)
		s.usage = &usage
	}
	if len(chunk.Choices) == 0 {
		return
	}
	choice := chunk.Choices[0]
	delta := choice.Delta

	if reasoning := reasoningContentOf(delta.JSON.ExtraFields); reasoning != "" {
		s.pending = append(s.pending, streamRaw{kind: streamRawReasoning, part: s.reasoningIndex(), text: reasoning})
	}
	if delta.Content != "" {
		s.pending = append(s.pending, streamRaw{kind: streamRawText, part: s.textIndex(), text: delta.Content})
	}
	for _, call := range delta.ToolCalls {
		part, exists := s.toolParts[call.Index]
		if !exists {
			part = s.assignPart()
			s.toolParts[call.Index] = part
			s.sawTools = true
		}
		s.pending = append(s.pending, streamRaw{
			kind: streamRawToolFragment,
			part: part,
			tool: streamRawTool{id: call.ID, name: call.Function.Name, argsFragment: call.Function.Arguments},
		})
	}
	if choice.FinishReason != "" {
		finish, err := mapFinishReason(choice.FinishReason)
		if err != nil {
			s.finishErr = err
		} else {
			s.finish = finish
		}
	}
}

// end emits the terminal event exactly once: the recorded finish reason
// (defaulting to completed, or tool_calls when calls streamed without an
// explicit reason) plus the usage chunk's accounting. An interruption
// (insufficient_system_resource) surfaces as an error on the next Next
// call instead of a finish event — there is no truthful terminal state.
func (s *chatStream) end() {
	if s.ended {
		return
	}
	s.ended = true
	if s.finishErr != nil {
		return
	}
	finish := s.finish
	if finish == "" && s.sawTools {
		finish = inference.FinishToolCalls
	}
	if finish == "" {
		finish = inference.FinishCompleted
	}
	s.pending = append(s.pending, streamRaw{
		kind:       streamRawFinish,
		finish:     finish,
		usage:      s.usage,
		responseID: s.id,
	})
}

func (s *chatStream) assignPart() int {
	part := s.nextPart
	s.nextPart++
	return part
}

func (s *chatStream) reasoningIndex() int {
	if s.reasoningPart < 0 {
		s.reasoningPart = s.assignPart()
	}
	return s.reasoningPart
}

func (s *chatStream) textIndex() int {
	if s.textPart < 0 {
		s.textPart = s.assignPart()
	}
	return s.textPart
}

// decodeGenerateStream is the pure stage: raw stream events become
// canonical stream events.
func decodeGenerateStream(_ context.Context, raw streamRaw) (inference.GenerateStreamEvent, error) {
	switch raw.kind {
	case streamRawText:
		return inference.GenerateStreamEvent{
			PartIndex: raw.part,
			Delta:     inference.TextPartDelta{Text: raw.text},
		}, nil
	case streamRawReasoning:
		return inference.GenerateStreamEvent{
			PartIndex: raw.part,
			Delta:     inference.ReasoningDelta{Text: raw.text},
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
	default:
		return inference.GenerateStreamEvent{}, errdefs.Internalf("deepseek: unknown stream raw kind %d", raw.kind)
	}
}
