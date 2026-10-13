package dashscope

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
)

// generateStreamKind names what one decoded frame carries. A frame can carry
// more than one thing, so the transport queues one raw per contribution and
// the decoder stays pure.
type generateStreamKind int

const (
	generateStreamText generateStreamKind = iota
	generateStreamReasoning
	generateStreamTool
	generateStreamSearch
	generateStreamFinish
)

// generateStreamRaw is one decoded stream frame as the transport hands it to
// the decoder: canonical part indices are already assigned, because only the
// transport sees the frames in order.
type generateStreamRaw struct {
	kind      generateStreamKind
	part      int
	text      string
	toolID    string
	toolName  string
	toolArgs  string
	finish    inference.FinishReason
	usage     *inference.Usage
	requestID string
	search    *SearchOutput
}

// generateStreamFrame is one event payload of the streamed surface. The
// platform reports refusals in band — code and message inside an HTTP 200
// stream — so the error fields are part of the frame.
type generateStreamFrame struct {
	Output struct {
		Choices    []generateChoice    `json:"choices"`
		SearchInfo *generateSearchInfo `json:"search_info"`
	} `json:"output"`
	Usage     *generateUsage `json:"usage"`
	RequestID string         `json:"request_id"`
	Code      string         `json:"code"`
	Message   string         `json:"message"`
}

// transportGenerateStream opens the event stream. The request carries the sse
// header, which is what turns the answer into a sequence of events; a refusal
// the platform reports before the stream opens is already classified by the
// request path.
func transportGenerateStream(
	client *mediaClient,
) inference.Transport[generateWire, inference.ProviderStream[generateStreamRaw]] {
	return func(
		ctx context.Context,
		wire generateWire,
	) (inference.ProviderStream[generateStreamRaw], error) {
		resp, err := client.request(
			ctx, http.MethodPost, wire.path, generateRequest(wire),
			requestOptions{sse: true},
		)
		if err != nil {
			inference.LogProviderStream(ctx, providerID, "generate", wire.model, err, "")
			return nil, err
		}
		stream := &generateEventStream{
			body:      resp.Body,
			reader:    bufio.NewReaderSize(resp.Body, 64<<10),
			requestID: resp.Header.Get("x-request-id"),
			parts:     make(map[string]int),
		}
		inference.LogProviderStream(
			ctx, providerID, "generate", wire.model, nil, stream.requestID)
		return stream, nil
	}
}

// generateEventStream is the stateful half of the streaming pipeline: it
// reads the event stream, folds each frame into the raw fragments it carries,
// and assigns canonical part indices in arrival order, so the decoder stays
// pure and concurrent-safe.
//
// The stream assumes incremental frames — the compiler always asks for them —
// so a text fragment is appended verbatim; the reasoning trace, the text, and
// each tool call are tracked as separate parts keyed by arrival.
type generateEventStream struct {
	body      io.ReadCloser
	reader    *bufio.Reader
	requestID string

	parts    map[string]int
	nextPart int

	pending  []generateStreamRaw
	terminal bool
}

func (s *generateEventStream) RequestID() string  { return s.requestID }
func (s *generateEventStream) ResponseID() string { return "" }

func (s *generateEventStream) Close() error {
	if s.body == nil {
		return nil
	}
	return s.body.Close()
}

func (s *generateEventStream) Next(
	ctx context.Context,
) (generateStreamRaw, error) {
	if err := ctx.Err(); err != nil {
		return generateStreamRaw{}, err
	}
	for len(s.pending) == 0 {
		if s.terminal {
			// The terminal frame is delivered; whatever follows — the
			// closing marker, or a clean close — ends the read.
			if _, err := s.readEvent(); err != nil {
				return generateStreamRaw{}, err
			}
			continue
		}
		payload, err := s.readEvent()
		if err != nil {
			return generateStreamRaw{}, err
		}
		if err := s.frames(payload); err != nil {
			return generateStreamRaw{}, err
		}
	}
	raw := s.pending[0]
	s.pending = s.pending[1:]
	return raw, nil
}

// readEvent reads one event payload: data lines accumulate until a blank line
// closes the event, the way the event-stream format specifies. Framing the
// platform adds — heartbeats and comments, id, event, and retry — is skipped,
// and the closing [DONE] marker reads as end of stream.
func (s *generateEventStream) readEvent() ([]byte, error) {
	var fragments []string
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("dashscope: read generate stream: %w", err)
		}
		atEnd := errors.Is(err, io.EOF)
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			if len(fragments) > 0 {
				return eventPayload(fragments)
			}
		case strings.HasPrefix(trimmed, "data:"):
			fragment := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if fragment == "[DONE]" {
				return nil, io.EOF
			}
			if fragment != "" {
				if len(fragment) > sseResponseLimit {
					return nil, fmt.Errorf(
						"dashscope: generate stream event exceeds %d bytes",
						sseResponseLimit,
					)
				}
				fragments = append(fragments, fragment)
			}
		case strings.HasPrefix(trimmed, ":"),
			strings.HasPrefix(trimmed, "event:"),
			strings.HasPrefix(trimmed, "id:"),
			strings.HasPrefix(trimmed, "retry:"):
			// Framing without content: a heartbeat, a comment, or the
			// stream's event name.
		default:
			return nil, fmt.Errorf(
				"dashscope: generate stream returned an unrecognized event line %q",
				trimmed,
			)
		}
		if atEnd {
			if len(fragments) > 0 {
				return eventPayload(fragments)
			}
			return nil, io.EOF
		}
	}
}

// eventPayload joins one event's data lines. The event-stream format joins
// them with newlines, and a newline between JSON tokens is whitespace.
func eventPayload(fragments []string) ([]byte, error) {
	return []byte(strings.Join(fragments, "\n")), nil
}

// frames turns one event payload into the raw fragments it carries, queued in
// the order they contribute to the answer.
func (s *generateEventStream) frames(payload []byte) error {
	var frame generateStreamFrame
	if err := json.Unmarshal(payload, &frame); err != nil {
		return fmt.Errorf("dashscope: decode generate stream frame: %w", err)
	}
	if frame.Code != "" {
		return errorBody{
			Code:      frame.Code,
			Message:   frame.Message,
			RequestID: frame.RequestID,
		}.failure("generate stream", http.StatusOK)
	}
	if frame.RequestID != "" {
		s.requestID = frame.RequestID
	}
	if output := frame.Output.SearchInfo.output(); output != nil {
		s.pending = append(s.pending, generateStreamRaw{
			kind:   generateStreamSearch,
			search: output,
		})
	}
	if len(frame.Output.Choices) == 0 {
		return nil
	}
	choice := frame.Output.Choices[0]
	if text := choice.Message.ReasoningContent; text != "" {
		s.pending = append(s.pending, generateStreamRaw{
			kind: generateStreamReasoning,
			part: s.part("reasoning"),
			text: text,
		})
	}
	content, err := decodeGenerateContent(choice.Message.Content)
	if err != nil {
		return err
	}
	if content != "" {
		s.pending = append(s.pending, generateStreamRaw{
			kind: generateStreamText,
			part: s.part("text"),
			text: content,
		})
	}
	for position, call := range choice.Message.ToolCalls {
		index := position
		if call.Index != nil {
			index = *call.Index
		}
		s.pending = append(s.pending, generateStreamRaw{
			kind:     generateStreamTool,
			part:     s.part(fmt.Sprintf("tool:%d", index)),
			toolID:   call.ID,
			toolName: call.Function.Name,
			toolArgs: call.Function.Arguments,
		})
	}
	if finish := choice.FinishReason; finish != "" && finish != "null" {
		raw := generateStreamRaw{
			kind:      generateStreamFinish,
			finish:    generateFinish(finish, len(choice.Message.ToolCalls) > 0),
			requestID: s.requestID,
		}
		if frame.Usage != nil {
			usage := frame.Usage.canonical()
			raw.usage = &usage
		}
		s.pending = append(s.pending, raw)
		s.terminal = true
	}
	return nil
}

// part returns the canonical index of one logical stream part, assigning the
// next index the first time the part appears, so fragments of one part fold
// together and the parts keep their arrival order.
func (s *generateEventStream) part(key string) int {
	if index, ok := s.parts[key]; ok {
		return index
	}
	index := s.nextPart
	s.nextPart++
	s.parts[key] = index
	return index
}

// decodeGenerateStream lowers one raw fragment. The raw already carries the
// canonical part index the accumulator assembles it under.
func decodeGenerateStream(
	_ context.Context,
	raw generateStreamRaw,
) (inference.GenerateStreamEvent, error) {
	switch raw.kind {
	case generateStreamText:
		return inference.GenerateStreamEvent{
			PartIndex: raw.part,
			Delta:     inference.TextPartDelta{Text: raw.text},
		}, nil
	case generateStreamReasoning:
		return inference.GenerateStreamEvent{
			PartIndex: raw.part,
			Delta:     inference.ReasoningDelta{Text: raw.text},
		}, nil
	case generateStreamTool:
		return inference.GenerateStreamEvent{
			PartIndex: raw.part,
			Delta: inference.ToolCallDelta{
				ID:                raw.toolID,
				Name:              raw.toolName,
				ArgumentsFragment: raw.toolArgs,
			},
		}, nil
	case generateStreamSearch:
		event := inference.GenerateStreamEvent{}
		if raw.search != nil {
			event.ProviderOutputs = inference.ProviderOutputs{raw.search}
		}
		return event, nil
	case generateStreamFinish:
		event := inference.GenerateStreamEvent{
			FinishReason: raw.finish,
			RequestID:    raw.requestID,
		}
		if raw.usage != nil {
			event.Usage = raw.usage
		}
		return event, nil
	default:
		return inference.GenerateStreamEvent{}, fmt.Errorf(
			"dashscope: unknown generate stream fragment %d", raw.kind)
	}
}
