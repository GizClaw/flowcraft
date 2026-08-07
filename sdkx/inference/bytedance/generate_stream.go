package bytedance

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	arkresponses "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model/responses"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime/utils"
)

// responsesStream adapts the ark SSE reader to ProviderStream[streamRaw]. It
// is the stateful stage of the streaming pipeline: it assigns canonical part
// indices to ark output items and collapses the API's snapshot-style events
// into deltas, so the decoder function stays pure.
type responsesStream struct {
	reader *utils.ResponsesStreamReader

	parts    map[int64]*streamPart // ark output index → canonical part
	nextPart int
	finished bool
	sawTools bool
}

type streamPart struct {
	index        int
	tool         bool
	reasoning    bool
	id           string // reasoning item id from output_item.added
	sawArgsDelta bool
	sawSummary   bool
}

func transportGenerateStream(
	client *arkruntime.Client,
) inference.Transport[generateWire, inference.ProviderStream[streamRaw]] {
	return func(
		ctx context.Context,
		wire generateWire,
	) (inference.ProviderStream[streamRaw], error) {
		reader, err := client.CreateResponsesStream(ctx, wireToArk(wire))
		if err != nil {
			return nil, classifyError(err)
		}
		return &responsesStream{
			reader: reader,
			parts:  make(map[int64]*streamPart),
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
		return streamRaw{}, errdefs.FromContext(err)
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

// apply folds one ark event into a streamRaw. keep=false means the event was
// bookkeeping-only (part registration, lifecycle pings) and the loop should
// read on. The event union has no shared discriminator, so dispatch follows
// accessor presence plus each payload's Type field.
func (s *responsesStream) apply(
	event *arkresponses.Event,
) (streamRaw, bool, error) {
	if failure := event.GetError(); failure != nil {
		return streamRaw{}, false, classifyResponseError(
			failure.GetCode(),
			failure.GetMessage(),
		)
	}
	if completed := event.GetResponseCompleted(); completed != nil {
		return s.applyTerminal(completed.GetResponse(), "")
	}
	if failed := event.GetResponseFailed(); failed != nil {
		response := failed.GetResponse()
		if failure := response.GetError(); failure != nil {
			return streamRaw{}, false, classifyResponseError(
				failure.GetCode(),
				failure.GetMessage(),
			)
		}
		return streamRaw{}, false, errdefs.NotAvailablef(
			"bytedance: response failed without detail",
		)
	}
	if incomplete := event.GetResponseIncomplete(); incomplete != nil {
		response := incomplete.GetResponse()
		reason := response.GetIncompleteDetails().GetReason()
		finish := arkIncompleteFinish(reason)
		if finish == "" {
			return streamRaw{}, false, errdefs.NotAvailablef(
				"bytedance: response incomplete: %s",
				reason,
			)
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
		// item.done is the reasoning terminal: it carries the full summary
		// when no text deltas streamed, plus the item id.
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
			// Nothing visible ever streamed: an id-only terminal would
			// accumulate into an empty reasoning part, which the canonical
			// contract rejects.
			return streamRaw{}, false, nil
		}
		return streamRaw{
			kind: streamRawReasoning,
			part: part.index,
			text: text,
			id:   part.id,
		}, true, nil
	}
	if text := event.GetText(); text != nil {
		if text.GetType() != arkresponses.EventType_response_output_text_delta {
			return streamRaw{}, false, nil // output_text.done: skip snapshots
		}
		part := s.registerPart(text.GetOutputIndex(), false)
		if text.GetDelta() == "" {
			return streamRaw{}, false, nil
		}
		return streamRaw{
			kind: streamRawText,
			part: part.index,
			text: text.GetDelta(),
		}, true, nil
	}
	if summary := event.GetReasoningText(); summary != nil {
		if summary.GetType() != arkresponses.EventType_response_reasoning_summary_text_delta {
			return streamRaw{}, false, nil // summary .done snapshots: skip
		}
		part := s.registerPart(summary.GetOutputIndex(), false)
		part.reasoning = true
		part.sawSummary = true
		if summary.GetDelta() == "" {
			return streamRaw{}, false, nil
		}
		return streamRaw{
			kind: streamRawReasoning,
			part: part.index,
			text: summary.GetDelta(),
		}, true, nil
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
	// Reasoning part markers, web-search lifecycle, and transcription
	// events are bookkeeping for this operation's output.
	return streamRaw{}, false, nil
}

// applyTerminal renders the completed/incomplete terminal event. An empty
// finishOverride means completed; the saw-tools rule picks the reason.
func (s *responsesStream) applyTerminal(
	response *arkresponses.ResponseObject,
	finishOverride inference.FinishReason,
) (streamRaw, bool, error) {
	finish := finishOverride
	if finish == "" {
		finish = inference.FinishCompleted
		if s.sawTools {
			finish = inference.FinishToolCalls
		}
	}
	raw, err := s.finishEvent(response, finish)
	return raw, err == nil, err
}

// registerPart assigns a stable canonical part index per ark output index.
// Text output (the answer message) lands before tool calls in the canonical
// response only when ark emits it first; the runtime assembles parts in
// index order, which mirrors wire order here.
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

// finishEvent renders the single terminal event. A duplicate terminal event
// would violate the runtime's single-finish invariant, so it is an error.
func (s *responsesStream) finishEvent(
	response *arkresponses.ResponseObject,
	finish inference.FinishReason,
) (streamRaw, error) {
	if s.finished {
		return streamRaw{}, errdefs.Internalf(
			"bytedance: stream emitted a duplicate terminal event",
		)
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
		return inference.FinishMaxOutput
	case "content_filter":
		return inference.FinishContentFilter
	}
	return ""
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
				Text: raw.text,
				ID:   raw.id,
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
		"bytedance: unknown stream raw kind %d",
		raw.kind,
	)
}
