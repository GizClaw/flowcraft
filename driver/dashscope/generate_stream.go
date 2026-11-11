package dashscope

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
)

type generateStreamKind int

const (
	generateStreamText generateStreamKind = iota
	generateStreamReasoning
	generateStreamTool
	generateStreamFinish
)

type generateStreamRaw struct {
	kind     generateStreamKind
	part     int
	text     string
	toolID   string
	toolName string
	toolArgs string
	finish   inference.FinishReason
	usage    *usage.Usage
}

type generateStreamFrame struct {
	Output struct {
		Choices []generateChoice `json:"choices"`
	} `json:"output"`
	Usage     *generateUsage `json:"usage"`
	RequestID string         `json:"request_id"`
	Code      string         `json:"code"`
	Message   string         `json:"message"`
}

type bufferedGenerateStream struct {
	raws []generateStreamRaw
	next int
}

func (s *bufferedGenerateStream) Next(context.Context) (generateStreamRaw, error) {
	if s.next >= len(s.raws) {
		return generateStreamRaw{}, io.EOF
	}
	raw := s.raws[s.next]
	s.next++
	return raw, nil
}

func (s *bufferedGenerateStream) Close() error { return nil }

func openGenerateStream(client *mediaClient) protocol.StreamOpener[inference.GenerateRequest, generateWire, generateStreamRaw] {
	return func(ctx context.Context, _ inference.GenerateRequest, wire generateWire) (protocol.RawStream[generateStreamRaw], error) {
		payloads, err := client.postSSEEvents(ctx, wire.path, generateRequest(wire), true)
		if err != nil {
			return nil, err
		}
		stream := &bufferedGenerateStream{}
		parts := map[string]int{}
		nextPart := 0
		part := func(key string) int {
			if index, ok := parts[key]; ok {
				return index
			}
			index := nextPart
			nextPart++
			parts[key] = index
			return index
		}
		for _, payload := range payloads {
			var frame generateStreamFrame
			if err := json.Unmarshal(payload, &frame); err != nil {
				return nil, fmt.Errorf("dashscope: decode stream frame: %w", err)
			}
			if frame.Code != "" {
				return nil, errorBody{Code: frame.Code, Message: frame.Message, RequestID: frame.RequestID}.failure("generate stream", 200)
			}
			if len(frame.Output.Choices) == 0 {
				continue
			}
			choice := frame.Output.Choices[0]
			if choice.Message.ReasoningContent != "" {
				stream.raws = append(stream.raws, generateStreamRaw{kind: generateStreamReasoning, part: part("reasoning"), text: choice.Message.ReasoningContent})
			}
			content, err := decodeGenerateContent(choice.Message.Content)
			if err != nil {
				return nil, err
			}
			if content != "" {
				stream.raws = append(stream.raws, generateStreamRaw{kind: generateStreamText, part: part("text"), text: content})
			}
			for position, call := range choice.Message.ToolCalls {
				index := position
				if call.Index != nil {
					index = *call.Index
				}
				stream.raws = append(stream.raws, generateStreamRaw{kind: generateStreamTool, part: part(fmt.Sprintf("tool:%d", index)), toolID: call.ID, toolName: call.Function.Name, toolArgs: call.Function.Arguments})
			}
			if choice.FinishReason != "" && choice.FinishReason != "null" {
				raw := generateStreamRaw{kind: generateStreamFinish, finish: generateFinish(choice.FinishReason, len(choice.Message.ToolCalls) > 0)}
				if frame.Usage != nil {
					accounting := frame.Usage.canonical()
					raw.usage = &accounting
				}
				stream.raws = append(stream.raws, raw)
			}
		}
		return stream, nil
	}
}

func newGenerateStreamDecoder(_ inference.GenerateRequest) protocol.StreamDecoder[generateStreamRaw] {
	started := false
	return func(_ context.Context, raw generateStreamRaw) ([]inference.StreamEvent, error) {
		var events []inference.StreamEvent
		if !started {
			started = true
			events = append(events, inference.StreamEvent{Kind: inference.StreamStart})
		}
		switch raw.kind {
		case generateStreamText:
			events = append(events, inference.StreamEvent{Kind: inference.StreamText, Index: raw.part, Text: raw.text})
		case generateStreamReasoning:
			events = append(events, inference.StreamEvent{Kind: inference.StreamReasoning, Index: raw.part, Text: raw.text})
		case generateStreamTool:
			events = append(events, inference.StreamEvent{Kind: inference.StreamToolCall, Index: raw.part,
				Tool: &inference.ToolCallDelta{ID: raw.toolID, Name: raw.toolName, Arguments: raw.toolArgs}})
		case generateStreamFinish:
			if raw.usage != nil {
				events = append(events, inference.StreamEvent{Kind: inference.StreamUsage, Usage: raw.usage})
			}
			events = append(events, inference.StreamEvent{Kind: inference.StreamEnd, Finish: raw.finish})
		default:
			return nil, fmt.Errorf("dashscope: unknown stream fragment %d", raw.kind)
		}
		return events, nil
	}
}
