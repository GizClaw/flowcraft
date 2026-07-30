package qwen

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

// streamFragment is the raw stream event: one indivisible delta extracted
// from an SSE chunk. Chunks can bundle a reasoning delta, a text delta,
// several tool-call deltas, and the finish marker, so the raw stream
// splits them and serves fragments one at a time — the decoder then maps
// fragments statelessly, one event per fragment. Fragments stay
// provider-typed (no canonical values): the decoder converts.
type streamFragment struct {
	kind   fragmentKind
	text   string        // reasoning or text delta
	call   *wireToolCall // tool-call delta
	finish string        // wire finish reason
	usage  *dashUsage
}

type fragmentKind int

const (
	fragmentReasoning fragmentKind = iota
	fragmentText
	fragmentToolCall
	fragmentFinish
)

// PartIndex slots are fixed so the decoder stays pure: reasoning occupies
// slot 0, text slot 1, tool calls slot 2+index. Unused slots simply never
// produce events; the accumulator assembles parts in slot order.
const (
	partSlotReasoning = 0
	partSlotText      = 1
	partSlotToolCalls = 2
)

// decodeStreamFragment maps one raw fragment onto one canonical event.
func decodeStreamFragment(
	_ context.Context,
	fragment streamFragment,
) (inference.GenerateStreamEvent, error) {
	switch fragment.kind {
	case fragmentReasoning:
		return inference.GenerateStreamEvent{
			PartIndex: partSlotReasoning,
			Delta:     inference.ReasoningDelta{Text: fragment.text},
		}, nil
	case fragmentText:
		return inference.GenerateStreamEvent{
			PartIndex: partSlotText,
			Delta:     inference.TextPartDelta{Text: fragment.text},
		}, nil
	case fragmentToolCall:
		if fragment.call == nil {
			return inference.GenerateStreamEvent{}, fmt.Errorf(
				"qwen: tool-call fragment carries no call",
			)
		}
		index := 0
		if fragment.call.Index != nil {
			index = *fragment.call.Index
		}
		return inference.GenerateStreamEvent{
			PartIndex: partSlotToolCalls + index,
			Delta: inference.ToolCallDelta{
				ID:                fragment.call.ID,
				Name:              fragment.call.Function.Name,
				ArgumentsFragment: fragment.call.Function.Arguments,
			},
		}, nil
	case fragmentFinish:
		event := inference.GenerateStreamEvent{
			FinishReason: finishReason(fragment.finish),
		}
		if fragment.usage != nil {
			usage := fragment.usage.canonical()
			event.Usage = &usage
		}
		return event, nil
	}
	return inference.GenerateStreamEvent{}, fmt.Errorf(
		"qwen: unknown stream fragment kind %d", fragment.kind,
	)
}

// sseStream is the ProviderStream over a DashScope event-stream body: it
// scans "data:" chunks, decodes each into the shared envelope, and queues
// the extracted fragments for pull consumption.
type sseStream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	queue   []streamFragment
	done    bool
}

func transportGenerateStream(
	client *dashClient,
) inference.Transport[generateWire, inference.ProviderStream[streamFragment]] {
	return func(
		ctx context.Context,
		wire generateWire,
	) (inference.ProviderStream[streamFragment], error) {
		defaultPreserveThinking(&wire)
		body, err := client.postSSE(ctx, wire.Path, wire)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
		return &sseStream{body: body, scanner: scanner}, nil
	}
}

func (s *sseStream) Next(_ context.Context) (streamFragment, error) {
	for len(s.queue) == 0 {
		if s.done {
			return streamFragment{}, io.EOF
		}
		if !s.scanner.Scan() {
			s.done = true
			if err := s.scanner.Err(); err != nil {
				return streamFragment{}, fmt.Errorf(
					"qwen: read event stream: %w", err,
				)
			}
			return streamFragment{}, io.EOF
		}
		line := strings.TrimSpace(s.scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		fragments, err := splitChunk([]byte(payload))
		if err != nil {
			s.done = true
			return streamFragment{}, err
		}
		s.queue = fragments
	}
	fragment := s.queue[0]
	s.queue = s.queue[1:]
	return fragment, nil
}

func (s *sseStream) Close() error {
	s.done = true
	return s.body.Close()
}

// splitChunk decodes one SSE chunk into its fragments. DashScope streams
// incrementally (incremental_output=true), so message fields are deltas.
func splitChunk(payload []byte) ([]streamFragment, error) {
	var chunk dashResponse
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil, fmt.Errorf("qwen: decode stream chunk: %w", err)
	}
	if err := classifyEnvelope(chunk.Code, chunk.Message); err != nil {
		return nil, err
	}
	if len(chunk.Output.Choices) == 0 {
		return nil, nil
	}
	choice := chunk.Output.Choices[0]
	message := choice.Message
	var fragments []streamFragment
	if message.ReasoningContent != "" {
		fragments = append(fragments, streamFragment{
			kind: fragmentReasoning,
			text: message.ReasoningContent,
		})
	}
	if text := messageContentText(message.Content); text != "" {
		fragments = append(fragments, streamFragment{
			kind: fragmentText,
			text: text,
		})
	}
	for i := range message.ToolCalls {
		call := message.ToolCalls[i]
		fragments = append(fragments, streamFragment{
			kind: fragmentToolCall,
			call: &call,
		})
	}
	if choice.FinishReason != "" && choice.FinishReason != "null" {
		usage := chunk.usage()
		fragments = append(fragments, streamFragment{
			kind:   fragmentFinish,
			finish: choice.FinishReason,
			usage:  &usage,
		})
	}
	return fragments, nil
}
