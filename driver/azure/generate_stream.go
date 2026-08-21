package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"
)

// responsesStream adapts the SDK SSE reader to ProviderStream[streamRaw]. It
// is the stateful stage of the streaming pipeline: it assigns canonical part
// indices to output items and collapses the API's snapshot-style events into
// deltas, so the decoder function stays pure.
type responsesStream struct {
	stream *ssestream.Stream[responses.ResponseStreamEventUnion]

	parts    map[int64]*streamPart // output index → canonical part
	nextPart int
	finished bool
	sawTools bool

	// webSearchOutput is the cumulative provider output snapshot for the
	// hosted web_search tool. Stream events replace it as calls and
	// citations arrive.
	webSearchOutput WebSearchOutput
}

type streamPart struct {
	index           int
	tool            bool
	reasoning       bool
	id              string // reasoning item id from output_item.added
	sawArgsDelta    bool
	sawArgsSnapshot bool
	sawSummary      bool
}

func transportGenerateStream(
	client openai.Client,
) inference.Transport[generateWire, inference.ProviderStream[streamRaw]] {
	return func(
		ctx context.Context,
		wire generateWire,
	) (inference.ProviderStream[streamRaw], error) {
		stream := client.Responses.NewStreaming(ctx, wireToParams(wire))
		if err := stream.Err(); err != nil {
			classified := classifyError(err)
			logInferenceStream(ctx, "generate", wire.model, classified, "")
			return nil, classified
		}
		logInferenceStream(ctx, "generate", wire.model, nil, "")
		return &responsesStream{
			stream: stream,
			parts:  make(map[int64]*streamPart),
		}, nil
	}
}

func (s *responsesStream) Close() error {
	if s.stream == nil {
		return nil
	}
	return classifyError(s.stream.Close())
}

func (s *responsesStream) Next(ctx context.Context) (streamRaw, error) {
	if err := ctx.Err(); err != nil {
		return streamRaw{}, errdefs.FromContext(err)
	}
	for {
		if !s.stream.Next() {
			if err := s.stream.Err(); err != nil {
				classified := classifyError(err)
				logInferenceStream(ctx, "generate", "", classified, "")
				return streamRaw{}, classified
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
// was bookkeeping-only (part registration, lifecycle pings) and the loop
// should read on.
func (s *responsesStream) apply(
	event responses.ResponseStreamEventUnion,
) (streamRaw, bool, error) {
	switch event.Type {
	case "error":
		return streamRaw{}, false, classifyResponseError(event.Code, event.Message)
	case "response.failed":
		if event.Response.Error.Message != "" {
			return streamRaw{}, false, classifyResponseError(
				string(event.Response.Error.Code),
				event.Response.Error.Message,
			)
		}
		return streamRaw{}, false, errdefs.NotAvailablef(
			"azure: response failed without detail",
		)
	case "response.completed":
		return s.applyTerminal(&event.Response, inference.FinishCompleted)
	case "response.incomplete":
		return s.applyTerminal(
			&event.Response,
			incompleteFinish(event.Response.IncompleteDetails.Reason),
		)
	case "response.output_item.added", "response.output_item.done":
		switch event.Item.Type {
		case "reasoning":
			part := s.registerPart(event.OutputIndex, false)
			part.reasoning = true
			if event.Item.ID != "" {
				part.id = event.Item.ID
			}
			if event.Type == "response.output_item.added" {
				return streamRaw{}, false, nil
			}
			// item.done is the reasoning terminal: it carries the encrypted
			// payload, and the full summary when no text deltas streamed.
			raw := streamRaw{
				kind:      streamRawReasoning,
				part:      part.index,
				signature: event.Item.EncryptedContent,
				id:        part.id,
			}
			if !part.sawSummary {
				raw.text = reasoningSummary(event.Item.Summary)
			}
			if raw.text == "" && raw.signature == "" {
				return streamRaw{}, false, nil
			}
			return raw, true, nil
		case "function_call":
		case "web_search_call":
			s.addWebSearchCall(openaiWebSearchCall(event.Item))
			if event.Type == "response.output_item.done" {
				return s.webSearchSnapshot()
			}
			return streamRaw{}, false, nil
		default:
			return streamRaw{}, false, nil
		}
		part := s.registerPart(event.OutputIndex, true)
		raw := streamRaw{
			kind: streamRawToolFragment,
			part: part.index,
			tool: streamRawTool{id: event.Item.CallID, name: event.Item.Name},
		}
		// item.done carries the complete arguments; emit them only when no
		// incremental deltas streamed for this part, otherwise the runtime
		// accumulator would append the snapshot a second time.
		if event.Type == "response.output_item.done" &&
			!part.sawArgsDelta && !part.sawArgsSnapshot {
			raw.tool.argsFragment = event.Item.Arguments.OfString
			if event.Item.Arguments.OfString != "" {
				part.sawArgsSnapshot = true
			}
		}
		return raw, true, nil
	case "response.reasoning_summary_text.delta":
		part := s.registerPart(event.OutputIndex, false)
		part.reasoning = true
		part.sawSummary = true
		if event.Delta == "" {
			return streamRaw{}, false, nil
		}
		return streamRaw{
			kind: streamRawReasoning,
			part: part.index,
			text: event.Delta,
		}, true, nil
	case "response.output_text.delta":
		part := s.registerPart(event.OutputIndex, false)
		if event.Delta == "" {
			return streamRaw{}, false, nil
		}
		return streamRaw{
			kind: streamRawText,
			part: part.index,
			text: event.Delta,
		}, true, nil
	case "response.function_call_arguments.delta":
		part := s.registerPart(event.OutputIndex, true)
		part.sawArgsDelta = true
		if event.Delta == "" {
			return streamRaw{}, false, nil
		}
		return streamRaw{
			kind: streamRawToolFragment,
			part: part.index,
			tool: streamRawTool{argsFragment: event.Delta},
		}, true, nil
	case "response.function_call_arguments.done":
		part := s.registerPart(event.OutputIndex, true)
		if part.sawArgsDelta || part.sawArgsSnapshot || event.Arguments == "" {
			return streamRaw{}, false, nil
		}
		part.sawArgsSnapshot = true
		return streamRaw{
			kind: streamRawToolFragment,
			part: part.index,
			tool: streamRawTool{argsFragment: event.Arguments},
		}, true, nil
	case "response.output_text.annotation.added":
		citation, ok := openaiAnnotationCitation(event.Annotation)
		if !ok {
			return streamRaw{}, false, nil
		}
		s.addCitation(citation)
		return s.webSearchSnapshot()
	}
	// Content-part markers, lifecycle pings, and audio events are
	// bookkeeping for this operation's output.
	return streamRaw{}, false, nil
}

// applyTerminal renders the completed/incomplete terminal event; the
// saw-tools rule picks the finish reason for a bare completion.
func (s *responsesStream) applyTerminal(
	response *responses.Response,
	finish inference.FinishReason,
) (streamRaw, bool, error) {
	if s.sawTools {
		finish = inference.FinishToolCalls
	}
	raw, err := s.finishEvent(response, finish)
	return raw, err == nil, err
}

// registerPart assigns a stable canonical part index per output index.
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
	response *responses.Response,
	finish inference.FinishReason,
) (streamRaw, error) {
	if s.finished {
		return streamRaw{}, errdefs.Internalf(
			"azure: stream emitted a duplicate terminal event",
		)
	}
	s.finished = true
	usage := responseUsage(response.Usage)
	raw := streamRaw{
		kind:       streamRawFinish,
		usage:      &usage,
		finish:     finish,
		responseID: response.ID,
	}
	if len(s.webSearchOutput.Calls) > 0 || len(s.webSearchOutput.Citations) > 0 {
		raw.providerOutputs = inference.ProviderOutputs{
			&WebSearchOutput{
				Calls:     append([]inference.WebSearchCall(nil), s.webSearchOutput.Calls...),
				Citations: append([]inference.Citation(nil), s.webSearchOutput.Citations...),
			},
		}
	}
	return raw, nil
}

func (s *responsesStream) addWebSearchCall(call inference.WebSearchCall) {
	for i := range s.webSearchOutput.Calls {
		if s.webSearchOutput.Calls[i].ID != call.ID {
			continue
		}
		if call.Status != "" {
			s.webSearchOutput.Calls[i].Status = call.Status
		}
		if call.Action != "" {
			s.webSearchOutput.Calls[i].Action = call.Action
		}
		if len(call.Queries) > 0 {
			s.webSearchOutput.Calls[i].Queries = call.Queries
		}
		if len(call.Sources) > 0 {
			s.webSearchOutput.Calls[i].Sources = call.Sources
		}
		return
	}
	s.webSearchOutput.Calls = append(s.webSearchOutput.Calls, call)
}

func (s *responsesStream) addCitation(citation inference.Citation) {
	for i := range s.webSearchOutput.Citations {
		existing := s.webSearchOutput.Citations[i]
		if existing.URL != citation.URL {
			continue
		}
		if existing.StartIndex == nil && citation.StartIndex == nil {
			return
		}
		if existing.StartIndex != nil && citation.StartIndex != nil &&
			*existing.StartIndex == *citation.StartIndex {
			return
		}
	}
	s.webSearchOutput.Citations = append(s.webSearchOutput.Citations, citation)
}

func (s *responsesStream) webSearchSnapshot() (streamRaw, bool, error) {
	if len(s.webSearchOutput.Calls) == 0 && len(s.webSearchOutput.Citations) == 0 {
		return streamRaw{}, false, nil
	}
	return streamRaw{
		kind: streamRawProviderOutput,
		providerOutputs: inference.ProviderOutputs{
			&WebSearchOutput{
				Calls:     append([]inference.WebSearchCall(nil), s.webSearchOutput.Calls...),
				Citations: append([]inference.Citation(nil), s.webSearchOutput.Citations...),
			},
		},
	}, true, nil
}

func openaiAnnotationCitation(annotation any) (inference.Citation, bool) {
	raw, err := json.Marshal(annotation)
	if err != nil {
		return inference.Citation{}, false
	}
	var union responses.ResponseOutputTextAnnotationUnion
	if err := json.Unmarshal(raw, &union); err != nil {
		return inference.Citation{}, false
	}
	if union.Type != "url_citation" {
		return inference.Citation{}, false
	}
	url := union.AsURLCitation()
	citation := inference.Citation{
		URL:   url.URL,
		Title: url.Title,
	}
	start, end := url.StartIndex, url.EndIndex
	citation.StartIndex = &start
	citation.EndIndex = &end
	return citation, true
}

// decodeGenerateStream is pure: streamRaw already carries canonical part
// indices assigned by the stateful transport.
func decodeGenerateStream(
	ctx context.Context,
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
				ID:        raw.id,
			},
		}, nil
	case streamRawProviderOutput:
		return inference.GenerateStreamEvent{
			ProviderOutputs: raw.providerOutputs.Clone(),
		}, nil
	case streamRawFinish:
		event := inference.GenerateStreamEvent{
			FinishReason:    raw.finish,
			ResponseID:      raw.responseID,
			ProviderOutputs: raw.providerOutputs.Clone(),
		}
		logInferenceStreamEnd(ctx, "generate", raw.responseID)
		if raw.usage != nil {
			usage := rawUsageCanonical(*raw.usage)
			event.Usage = &usage
		}
		return event, nil
	}
	return inference.GenerateStreamEvent{}, fmt.Errorf(
		"azure: unknown stream raw kind %d",
		raw.kind,
	)
}
