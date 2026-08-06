package kimi

import (
	"context"
	"encoding/json"
	"io"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
)

// chunkEnvelope is one streaming chunk: delta content, optional finish
// reason, and (on the final chunk, since the driver always requests
// stream_options.include_usage) the usage object.
type chunkEnvelope struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int64  `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usageWire `json:"usage"`
}

// streamRawKind enumerates the events the stateful stream transport hands
// to the pure stream decoder.
type streamRawKind int

const (
	streamRawText streamRawKind = iota
	streamRawReasoning
	streamRawToolFragment
	streamRawFinish
)

type streamRaw struct {
	kind   streamRawKind
	part   int // canonical part index, assigned by the transport
	text   string
	tool   streamRawTool
	usage  *rawUsage
	finish inference.FinishReason
}

type streamRawTool struct {
	id           string
	name         string
	argsFragment string
}

// chatStream is the stateful stage: it assigns canonical part indices as
// deltas arrive (reasoning precedes text on this surface), folds chunk
// fields into delta events, and holds the finish event back until the
// stream ends so usage rides along — Kimi delivers usage in its own chunk
// after the finish_reason chunk.
type chatStream struct {
	body    io.ReadCloser
	events  chan sseEvent
	cancel  context.CancelFunc
	pending []streamRaw

	reasoningPart int
	textPart      int
	toolParts     map[int64]int
	nextPart      int

	finish   inference.FinishReason
	usage    *rawUsage
	sawTools bool
	ended    bool
}

// transportGenerateStream opens the streaming request and returns the
// stateful provider stream.
func transportGenerateStream(client *kimiClient) inference.Transport[generateWire, inference.ProviderStream[streamRaw]] {
	return func(ctx context.Context, wire generateWire) (inference.ProviderStream[streamRaw], error) {
		wire.Stream = true
		request, err := client.newRequest(ctx, wire, true)
		if err != nil {
			return nil, err
		}
		response, err := client.http.Do(request)
		if err != nil {
			return nil, classifyError(err)
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			defer response.Body.Close()
			payload, _ := io.ReadAll(response.Body)
			return nil, classifyHTTPError(ctx, response.StatusCode, payload)
		}
		streamCtx, cancel := context.WithCancel(ctx)
		return &chatStream{
			body:          response.Body,
			events:        sseEvents(streamCtx, response.Body),
			cancel:        cancel,
			reasoningPart: -1,
			textPart:      -1,
			toolParts:     make(map[int64]int),
		}, nil
	}
}

func (s *chatStream) Close() error {
	s.cancel()
	return s.body.Close()
}

func (s *chatStream) Next(ctx context.Context) (streamRaw, error) {
	if err := ctx.Err(); err != nil {
		return streamRaw{}, errdefs.FromContext(err)
	}
	for len(s.pending) == 0 {
		if s.ended {
			return streamRaw{}, io.EOF
		}
		select {
		case event, ok := <-s.events:
			if !ok {
				s.end()
				continue
			}
			if err := s.apply(event.data); err != nil {
				return streamRaw{}, err
			}
		case <-ctx.Done():
			return streamRaw{}, errdefs.FromContext(ctx.Err())
		}
	}
	event := s.pending[0]
	s.pending = s.pending[1:]
	return event, nil
}

// apply folds one chunk into zero or more delta events. Finish reasons and
// usage are recorded, not emitted: the finish event ships at stream end so
// it carries both.
func (s *chatStream) apply(data []byte) error {
	var chunk chunkEnvelope
	if err := json.Unmarshal(data, &chunk); err != nil {
		return errdefs.NotAvailablef("kimi: decode stream chunk: %v", err)
	}
	if chunk.Usage != nil {
		usage := chunk.Usage.toRaw()
		s.usage = &usage
	}
	if len(chunk.Choices) == 0 {
		return nil
	}
	choice := chunk.Choices[0]
	delta := choice.Delta

	if delta.ReasoningContent != "" {
		s.pending = append(s.pending, streamRaw{kind: streamRawReasoning, part: s.reasoningIndex(), text: delta.ReasoningContent})
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
	if choice.FinishReason != nil && *choice.FinishReason != "" {
		finish, err := mapFinishReason(*choice.FinishReason)
		if err != nil {
			return err
		}
		s.finish = finish
	}
	return nil
}

// end emits the terminal event exactly once: the recorded finish reason
// (defaulting to completed, or tool_calls when calls streamed without an
// explicit reason) plus the usage chunk's accounting.
func (s *chatStream) end() {
	if s.ended {
		return
	}
	s.ended = true
	finish := s.finish
	if finish == "" && s.sawTools {
		finish = inference.FinishToolCalls
	}
	if finish == "" {
		finish = inference.FinishCompleted
	}
	s.pending = append(s.pending, streamRaw{kind: streamRawFinish, finish: finish, usage: s.usage})
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
		event := inference.GenerateStreamEvent{FinishReason: raw.finish}
		if raw.usage != nil {
			usage := rawUsageCanonical(*raw.usage)
			event.Usage = &usage
		}
		return event, nil
	default:
		return inference.GenerateStreamEvent{}, errdefs.Internalf("kimi: unknown stream raw kind %d", raw.kind)
	}
}
