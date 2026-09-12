package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

type generatePartAccumulator struct {
	kind message.PartKind

	text strings.Builder

	toolID        string
	toolName      string
	toolArguments strings.Builder

	audio         bytes.Buffer
	audioFormat   *media.AudioFormat
	audioDuration *int64
	completeImage *message.ImagePart
	imageInterim  bool

	reasoningSignature string
	reasoningID        string
}

type decodedGenerateStream[RawEvent any] struct {
	raw     ProviderStream[RawEvent]
	meta    ProviderStreamMetadata
	decode  GenerateStreamDecoder[RawEvent]
	model   ModelRef
	request GenerateRequest
	report  CompileReport
	parts   map[int]*generatePartAccumulator
	usage   Usage
	finish  FinishReason
	// finishSynthesized mirrors the terminal event's FinishSynthesized
	// flag; it is stamped onto the result so a fabricated finish reason
	// is visible to the caller.
	finishSynthesized bool
	requestID         string
	responseID        string
	outputs           ProviderOutputs
	startedAt         time.Time

	done      bool
	result    GenerateResponse
	resultErr error
}

func (s *decodedGenerateStream[RawEvent]) Next(
	ctx context.Context,
) (GenerateStreamEvent, error) {
	if s.done {
		return GenerateStreamEvent{}, io.EOF
	}
	rawEvent, err := s.raw.Next(ctx)
	if err != nil {
		if err == io.EOF {
			s.finishResult()
			if s.resultErr != nil {
				return GenerateStreamEvent{}, s.resultErr
			}
			return GenerateStreamEvent{}, io.EOF
		}
		s.done = true
		s.resultErr = s.streamProviderFailure("stream.read", err)
		return GenerateStreamEvent{}, s.resultErr
	}
	event, err := s.decode(ctx, rawEvent)
	if err != nil {
		s.done = true
		s.resultErr = s.streamError("stream.decode", err)
		return GenerateStreamEvent{}, s.resultErr
	}
	if event.Delta != nil {
		normalized, err := normalizePartDelta(event.Delta)
		if err != nil {
			s.done = true
			s.resultErr = s.streamError("stream.normalize", err)
			return GenerateStreamEvent{}, s.resultErr
		}
		event.Delta = normalized
	}
	if err := s.accumulate(event); err != nil {
		s.done = true
		s.resultErr = s.streamError(streamAccumulateDetail(err), err)
		return GenerateStreamEvent{}, s.resultErr
	}
	return event, nil
}

func (s *decodedGenerateStream[RawEvent]) Result() (GenerateResponse, error) {
	if !s.done {
		return GenerateResponse{}, s.streamError(
			"stream.result.premature",
			fmt.Errorf("stream is not complete"),
		)
	}
	return s.result.Clone(), s.resultErr
}

func (s *decodedGenerateStream[RawEvent]) Close() error {
	if err := s.raw.Close(); err != nil {
		return s.streamProviderFailure("stream.close", err)
	}
	return nil
}

// newStreamError builds the InvalidProviderResponse error used by the
// generate stream failure points. detail is a stable, redacted stage label
// (see Error.Detail) identifying where the failure was detected.
func newStreamError(detail string, cause error) *Error {
	out := NewError(InvalidProviderResponse, OperationGenerate, "", cause)
	out.Detail = detail
	if requestID, ok := errdefs.RequestID(out); ok {
		out.RequestID = requestID
	}
	return out
}

// streamProviderError builds a ProviderFailure error for a generate stream
// transport failure with the same stable stage label as newStreamError.
func streamProviderError(provider, detail string, cause error) *Error {
	out := newProviderError(OperationGenerate, provider, cause)
	out.Detail = detail
	return out
}

// providerID returns the provider identifier this stream can still attach
// when it ends early: the transport-level request id when provider metadata
// exposed one, otherwise the response id observed before truncation,
// otherwise an identifier carried by an earlier event.
func (s *decodedGenerateStream[RawEvent]) providerID() string {
	if s.meta != nil {
		if id := s.meta.RequestID(); id != "" {
			return id
		}
		if id := s.meta.ResponseID(); id != "" {
			return id
		}
	}
	return s.requestID
}

// withProviderID wraps cause with the stream's provider identifier unless
// the chain already carries one, so errdefs.RequestID consumers (span
// attributes, run event payloads) see it on failures raised after the
// provider stream opened.
func (s *decodedGenerateStream[RawEvent]) withProviderID(cause error) error {
	if id := s.providerID(); id != "" {
		if _, ok := errdefs.RequestID(cause); !ok {
			cause = errdefs.WithRequestID(cause, id)
		}
	}
	return cause
}

func (s *decodedGenerateStream[RawEvent]) streamError(
	detail string,
	cause error,
) *Error {
	return newStreamError(detail, s.withProviderID(cause))
}

func (s *decodedGenerateStream[RawEvent]) streamProviderFailure(
	detail string,
	cause error,
) *Error {
	return streamProviderError(s.model.ID.Provider, detail, s.withProviderID(cause))
}

// truncatedStreamError builds the ProviderTruncated error for a stream that
// ended without a terminal finish event, attaching any provider identifier
// the stream still knows.
func (s *decodedGenerateStream[RawEvent]) truncatedStreamError(cause error) *Error {
	out := NewError(ProviderTruncated, OperationGenerate, "", s.withProviderID(cause))
	out.Detail = "stream.finish.missing"
	if requestID, ok := errdefs.RequestID(out); ok {
		out.RequestID = requestID
	}
	return out
}

// toolCallConflictError marks an accumulation failure where incremental
// tool-call fragments contradict each other (a changed id or name). It lets
// Next attach a distinct Detail without parsing error text.
type toolCallConflictError struct {
	err error
}

func (e *toolCallConflictError) Error() string { return e.err.Error() }
func (e *toolCallConflictError) Unwrap() error { return e.err }

// streamAccumulateDetail picks the Detail for an accumulate failure.
func streamAccumulateDetail(err error) string {
	var conflict *toolCallConflictError
	if errors.As(err, &conflict) {
		return "stream.accumulate.tool_call"
	}
	return "stream.accumulate"
}

func (s *decodedGenerateStream[RawEvent]) accumulate(
	event GenerateStreamEvent,
) error {
	if event.Usage != nil {
		if err := event.Usage.Validate(); err != nil {
			return fmt.Errorf("generate stream usage: %w", err)
		}
	}
	if event.FinishReason != "" {
		if err := event.FinishReason.Validate(); err != nil {
			return err
		}
	}
	if event.FinishSynthesized && event.FinishReason == "" {
		return fmt.Errorf(
			"generate stream finish_synthesized requires a finish reason")
	}
	if err := event.ProviderOutputs.Validate(); err != nil {
		return err
	}
	if s.finish != "" && (event.Delta != nil || event.FinishReason != "") {
		return fmt.Errorf("stream emitted content after finish")
	}
	if event.Delta != nil {
		if event.PartIndex < 0 {
			return fmt.Errorf("generate part index must be non-negative")
		}
		if err := event.Delta.validateGenerateDelta(); err != nil {
			return err
		}
		part := s.parts[event.PartIndex]
		if part == nil {
			part = &generatePartAccumulator{kind: event.Delta.Kind()}
			s.parts[event.PartIndex] = part
		} else if part.kind != event.Delta.Kind() {
			return fmt.Errorf("generate part %d changed type", event.PartIndex)
		}
		if err := part.add(event.Delta); err != nil {
			return fmt.Errorf("generate part %d: %w", event.PartIndex, err)
		}
	}
	if event.Usage != nil {
		s.usage = event.Usage.Clone()
	}
	if event.RequestID != "" {
		s.requestID = event.RequestID
	}
	if event.ResponseID != "" {
		s.responseID = event.ResponseID
	}
	if len(event.ProviderOutputs) > 0 {
		for _, output := range event.ProviderOutputs {
			s.outputs.Replace(output.Clone())
		}
	}
	if event.FinishReason != "" {
		if s.finish != "" {
			return fmt.Errorf("stream emitted multiple finish reasons")
		}
		s.finish = event.FinishReason
		s.finishSynthesized = event.FinishSynthesized
	}
	return nil
}

func (p *generatePartAccumulator) add(delta PartDelta) error {
	switch value := delta.(type) {
	case TextPartDelta:
		p.text.WriteString(value.Text)
	case ToolCallDelta:
		if value.ID != "" {
			if p.toolID != "" && p.toolID != value.ID {
				return &toolCallConflictError{err: fmt.Errorf("tool call changed id")}
			}
			p.toolID = value.ID
		}
		if value.Name != "" {
			if p.toolName != "" && p.toolName != value.Name {
				return &toolCallConflictError{err: fmt.Errorf("tool call changed name")}
			}
			p.toolName = value.Name
		}
		p.toolArguments.WriteString(value.ArgumentsFragment)
	case AudioPartDelta:
		if value.Format != nil {
			if p.audioFormat != nil && *p.audioFormat != *value.Format {
				return fmt.Errorf("audio delta changed format")
			}
			format := *value.Format
			p.audioFormat = &format
		}
		if value.DurationMillis != nil {
			duration := *value.DurationMillis
			p.audioDuration = &duration
		}
		_, _ = p.audio.Write(value.Data)
	case ImagePartDelta:
		if !value.Interim && p.completeImage != nil && !p.imageInterim {
			return fmt.Errorf("image emitted more than one complete value")
		}
		image := value.Part
		p.completeImage = &image
		p.imageInterim = value.Interim
	case ReasoningDelta:
		p.text.WriteString(value.Text)
		if value.Signature != "" {
			p.reasoningSignature = value.Signature
		}
		if value.ID != "" {
			p.reasoningID = value.ID
		}
	default:
		return fmt.Errorf("unsupported generate part delta %T", delta)
	}
	return nil
}

func (s *decodedGenerateStream[RawEvent]) finishResult() {
	s.done = true
	if s.finish == "" {
		s.resultErr = s.truncatedStreamError(
			fmt.Errorf("stream ended without a finish reason"),
		)
		return
	}
	indices := make([]int, 0, len(s.parts))
	for index := range s.parts {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	parts := make([]message.Part, 0, len(indices))
	for _, index := range indices {
		part, err := s.parts[index].result()
		if err != nil {
			s.resultErr = s.streamError("stream.finish.part", err)
			return
		}
		parts = append(parts, part)
	}
	response := GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason:      s.finish,
		FinishSynthesized: s.finishSynthesized,
		Usage:             s.usage,
		ProviderOutputs:   s.outputs.Clone(),
	}
	metadata := s.report.Metadata(s.model)
	metadata.RequestID = s.requestID
	metadata.ResponseID = s.responseID
	response.Metadata = metadata
	deriveGenerateUsage(s.request, &response)
	// Stamp the call-context envelope at the terminal result, matching the
	// unary driver: the exact model reference that produced the stream and
	// the wall-clock latency from stream open to completion.
	response.Usage.Model = s.model
	response.Usage.LatencyMs = time.Since(s.startedAt).Milliseconds()
	if err := response.ValidateFor(s.request); err != nil {
		out := newResponseValidationError(OperationGenerate, s.withProviderID(err))
		out.Detail = terminalValidationDetail(out, response)
		if requestID, ok := errdefs.RequestID(out); ok {
			out.RequestID = requestID
		}
		s.resultErr = out
		return
	}
	s.result = response
}

// terminalValidationDetail picks the Detail for a terminal response
// validation failure. Structurally invalid tool-call parts and
// finish-reason/tool-call mismatches each get a distinct label (both mean
// the provider ended cleanly with a corrupt tool payload); undefined-tool
// calls keep their own label alongside the rejected call; everything else
// is a generic response validation failure.
func terminalValidationDetail(out *Error, response GenerateResponse) string {
	if out.Kind == UndefinedTool {
		return "stream.finish.undefined_tool"
	}
	for _, part := range response.Message.Content.Parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			continue
		}
		if call, ok := normalized.(message.ToolCallPart); ok {
			if err := call.Validate(); err != nil {
				return "stream.finish.tool_call"
			}
		}
	}
	hasToolCalls := response.Message.HasToolCalls()
	if (response.FinishReason == FinishToolCalls) != hasToolCalls {
		return "stream.finish.mismatch"
	}
	return "stream.finish.validation"
}

func (p *generatePartAccumulator) result() (message.Part, error) {
	switch p.kind {
	case message.PartText:
		return message.TextPart{Text: p.text.String()}, nil
	case message.PartToolCall:
		return message.ToolCallPart{Call: message.ToolCall{
			ID:        p.toolID,
			Name:      p.toolName,
			Arguments: json.RawMessage(p.toolArguments.String()),
		}}, nil
	case message.PartAudio:
		if p.audioFormat == nil {
			return nil, fmt.Errorf("streamed audio has no format")
		}
		source, err := media.NewAudioBytes(
			p.audio.Bytes(),
			p.audioFormat.Encoding.MediaType(),
		)
		if err != nil {
			return nil, err
		}
		duration := p.audioDuration
		if duration == nil {
			millis, ok := media.AudioDurationMillis(p.audio.Bytes(), *p.audioFormat)
			if ok {
				duration = &millis
			}
		}
		return message.AudioPart{
			Source:         source,
			Format:         ptr.Clone(p.audioFormat),
			DurationMillis: ptr.Clone(duration),
		}, nil
	case message.PartImage:
		if p.completeImage == nil {
			return nil, fmt.Errorf("streamed image is incomplete")
		}
		if p.imageInterim {
			// The stream ended on a progress snapshot. Returning it would
			// hand the caller a partially rendered image as the final
			// result, so the part index is reported as incomplete instead.
			return nil, fmt.Errorf("streamed image ended on an interim snapshot")
		}
		return p.completeImage.Clone(), nil
	case message.PartReasoning:
		part := message.ReasoningPart{
			Text:      p.text.String(),
			Signature: p.reasoningSignature,
			ID:        p.reasoningID,
		}
		if err := part.Validate(); err != nil {
			return nil, err
		}
		return part, nil
	default:
		return nil, fmt.Errorf("unknown generate part kind %q", p.kind)
	}
}
