package bytedance

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
	arkresponses "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model/responses"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime/utils"
)

type responsesStream struct {
	reader    *utils.ResponsesStreamReader
	requestID string
	parts     map[int64]*streamPart
	nextPart  int
	finished  bool
	sawTools  bool
}

type streamPart struct {
	index        int
	tool         bool
	reasoning    bool
	id           string
	sawArgsDelta bool
	sawSummary   bool
}

func openGenerateStream(
	client *arkruntime.Client,
	options []arkruntime.RequestOption,
) protocol.StreamOpener[inference.GenerateRequest, *arkresponses.ResponsesRequest, streamRaw] {
	return func(
		ctx context.Context,
		_ inference.GenerateRequest,
		request *arkresponses.ResponsesRequest,
	) (protocol.RawStream[streamRaw], error) {
		reader, err := client.CreateResponsesStream(ctx, request, options...)
		if err != nil {
			return nil, classifyError(err)
		}
		return &responsesStream{
			reader:    reader,
			requestID: reader.Header().Get(arkmodel.ClientRequestHeader),
			parts:     make(map[int64]*streamPart),
		}, nil
	}
}

func (s *responsesStream) Close() error {
	if s.reader == nil {
		return nil
	}
	return classifyError(s.reader.Close())
}

func (s *responsesStream) Next(ctx context.Context) (streamRaw, error) {
	if err := ctx.Err(); err != nil {
		return streamRaw{}, err
	}
	for {
		event, err := s.reader.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return streamRaw{}, io.EOF
			}
			return streamRaw{}, classifyError(err)
		}
		if event == nil {
			continue
		}
		raw, keep, err := s.apply(event)
		if err != nil {
			return streamRaw{}, err
		}
		if keep {
			return raw, nil
		}
	}
}

func (s *responsesStream) apply(event *arkresponses.Event) (streamRaw, bool, error) {
	if failure := event.GetError(); failure != nil {
		return streamRaw{}, false, classifyResponseError(failure.GetCode(), failure.GetMessage())
	}
	if completed := event.GetResponseCompleted(); completed != nil {
		return s.applyTerminal(completed.GetResponse(), "")
	}
	if failed := event.GetResponseFailed(); failed != nil {
		response := failed.GetResponse()
		if failure := response.GetError(); failure != nil {
			return streamRaw{}, false, classifyResponseError(failure.GetCode(), failure.GetMessage())
		}
		return streamRaw{}, false, errdefs.NotAvailablef("bytedance: response failed without detail")
	}
	if incomplete := event.GetResponseIncomplete(); incomplete != nil {
		response := incomplete.GetResponse()
		reason := response.GetIncompleteDetails().GetReason()
		finish := arkIncompleteFinish(reason)
		if finish == "" {
			return streamRaw{}, false, errdefs.NotAvailablef("bytedance: response incomplete: %s", reason)
		}
		return s.applyTerminal(response, finish)
	}
	if item := event.GetItem(); item != nil {
		if reasoning := item.GetItem().GetReasoning(); reasoning != nil {
			part := s.registerPart(item.GetOutputIndex(), false)
			part.reasoning = true
			if reasoning.GetId() != "" {
				part.id = reasoning.GetId()
			}
			return streamRaw{}, false, nil
		}
		call := item.GetItem().GetFunctionToolCall()
		if call == nil {
			return streamRaw{}, false, nil
		}
		part := s.registerPart(item.GetOutputIndex(), true)
		return streamRaw{
			kind: streamRawToolFragment,
			part: part.index,
			tool: streamRawTool{id: call.GetCallId(), name: call.GetName()},
		}, true, nil
	}
	if done := event.GetItemDone(); done != nil {
		reasoning := done.GetItem().GetReasoning()
		if reasoning == nil {
			return streamRaw{}, false, nil
		}
		part := s.registerPart(done.GetOutputIndex(), false)
		part.reasoning = true
		if reasoning.GetId() != "" {
			part.id = reasoning.GetId()
		}
		text := ""
		if !part.sawSummary {
			text = reasoningSummaryText(reasoning.GetSummary())
		}
		if text == "" && !part.sawSummary {
			return streamRaw{}, false, nil
		}
		return streamRaw{kind: streamRawReasoning, part: part.index, text: text, id: part.id}, true, nil
	}
	if text := event.GetText(); text != nil {
		if text.GetType() != arkresponses.EventType_response_output_text_delta {
			return streamRaw{}, false, nil
		}
		part := s.registerPart(text.GetOutputIndex(), false)
		if text.GetDelta() == "" {
			return streamRaw{}, false, nil
		}
		return streamRaw{kind: streamRawText, part: part.index, text: text.GetDelta()}, true, nil
	}
	if summary := event.GetReasoningText(); summary != nil {
		if summary.GetType() != arkresponses.EventType_response_reasoning_summary_text_delta {
			return streamRaw{}, false, nil
		}
		part := s.registerPart(summary.GetOutputIndex(), false)
		part.reasoning = true
		part.sawSummary = true
		if summary.GetDelta() == "" {
			return streamRaw{}, false, nil
		}
		return streamRaw{kind: streamRawReasoning, part: part.index, text: summary.GetDelta()}, true, nil
	}
	if fragment := event.GetFunctionCallArguments(); fragment != nil {
		part := s.registerPart(fragment.GetOutputIndex(), true)
		switch fragment.GetType() {
		case arkresponses.EventType_response_function_call_arguments_delta:
			part.sawArgsDelta = true
			if fragment.GetDelta() == "" {
				return streamRaw{}, false, nil
			}
			return streamRaw{
				kind: streamRawToolFragment,
				part: part.index,
				tool: streamRawTool{argsFragment: fragment.GetDelta()},
			}, true, nil
		case arkresponses.EventType_response_function_call_arguments_done:
			if part.sawArgsDelta || fragment.GetArguments() == "" {
				return streamRaw{}, false, nil
			}
			return streamRaw{
				kind: streamRawToolFragment,
				part: part.index,
				tool: streamRawTool{argsFragment: fragment.GetArguments()},
			}, true, nil
		}
		return streamRaw{}, false, nil
	}
	return streamRaw{}, false, nil
}

func (s *responsesStream) applyTerminal(
	response *arkresponses.ResponseObject,
	finishOverride inference.FinishReason,
) (streamRaw, bool, error) {
	finish := finishOverride
	if finish == "" {
		finish = inference.FinishStop
		if s.sawTools {
			finish = inference.FinishToolCalls
		}
	}
	raw, err := s.finishEvent(response, finish)
	return raw, err == nil, err
}

func (s *responsesStream) registerPart(outputIndex int64, tool bool) *streamPart {
	part, ok := s.parts[outputIndex]
	if !ok {
		part = &streamPart{index: s.nextPart, tool: tool}
		s.nextPart++
		s.parts[outputIndex] = part
	}
	if tool {
		s.sawTools = true
	}
	return part
}

func (s *responsesStream) finishEvent(
	response *arkresponses.ResponseObject,
	finish inference.FinishReason,
) (streamRaw, error) {
	if s.finished {
		return streamRaw{}, errdefs.Internalf("bytedance: stream emitted a duplicate terminal event")
	}
	s.finished = true
	usage := arkUsage(response.GetUsage())
	return streamRaw{
		kind:       streamRawFinish,
		usage:      &usage,
		finish:     finish,
		responseID: response.GetId(),
	}, nil
}

func arkIncompleteFinish(reason string) inference.FinishReason {
	switch reason {
	case "max_output_tokens", "max_tokens":
		return inference.FinishLength
	case "content_filter":
		return inference.FinishContentFilter
	}
	return ""
}

func newGenerateStreamDecoder(
	_ inference.GenerateRequest,
) protocol.StreamDecoder[streamRaw] {
	started := false
	return func(_ context.Context, raw streamRaw) ([]inference.StreamEvent, error) {
		var events []inference.StreamEvent
		if !started {
			started = true
			events = append(events, inference.StreamEvent{Kind: inference.StreamStart})
		}
		switch raw.kind {
		case streamRawText:
			events = append(events, inference.StreamEvent{
				Kind: inference.StreamText, Index: raw.part, Text: raw.text,
			})
		case streamRawToolFragment:
			events = append(events, inference.StreamEvent{
				Kind:  inference.StreamToolCall,
				Index: raw.part,
				Tool: &inference.ToolCallDelta{
					ID: raw.tool.id, Name: raw.tool.name, Arguments: raw.tool.argsFragment,
				},
			})
		case streamRawReasoning:
			events = append(events, inference.StreamEvent{
				Kind: inference.StreamReasoning, Index: raw.part, Text: raw.text, ID: raw.id,
			})
		case streamRawFinish:
			if raw.usage != nil {
				accounting := rawUsageCanonical(*raw.usage)
				events = append(events, inference.StreamEvent{
					Kind: inference.StreamUsage, Usage: &accounting,
				})
			}
			events = append(events, inference.StreamEvent{
				Kind: inference.StreamEnd, Finish: raw.finish,
			})
		default:
			return nil, fmt.Errorf("bytedance: unknown stream raw kind %d", raw.kind)
		}
		return events, nil
	}
}
