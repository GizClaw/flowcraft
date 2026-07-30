package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference/media"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// PartDelta is the sealed provider-neutral union accepted by GenerateStream.
type PartDelta interface {
	Kind() PartKind
	validateGenerateDelta() error
	inferencePartDelta()
}

type TextPartDelta struct {
	Text string `json:"text"`
}

func (TextPartDelta) Kind() PartKind               { return PartText }
func (TextPartDelta) validateGenerateDelta() error { return nil }
func (TextPartDelta) inferencePartDelta()          {}

// ToolCallDelta carries provider-neutral incremental tool-call arguments.
// ArgumentsFragment is validated only after stream accumulation.
type ToolCallDelta struct {
	ID                string `json:"id,omitempty"`
	Name              string `json:"name,omitempty"`
	ArgumentsFragment string `json:"arguments_fragment,omitempty"`
}

func (ToolCallDelta) Kind() PartKind               { return PartToolCall }
func (ToolCallDelta) validateGenerateDelta() error { return nil }
func (ToolCallDelta) inferencePartDelta()          {}

// ReasoningDelta carries incremental reasoning text. Signature and ID are
// terminal-only: providers sign a reasoning block when it completes, so the
// last delta for a part carries the opaque verification payload and the
// provider-issued trace identifier. The accumulator concatenates Text and
// keeps the latest Signature and ID.
type ReasoningDelta struct {
	Text      string `json:"text,omitempty"`
	Signature string `json:"signature,omitempty"`
	ID        string `json:"id,omitempty"`
}

func (ReasoningDelta) Kind() PartKind { return PartReasoning }
func (d ReasoningDelta) validateGenerateDelta() error {
	if d.Text == "" && d.Signature == "" && d.ID == "" {
		return fmt.Errorf("reasoning delta carries neither text, signature, nor id")
	}
	return nil
}
func (ReasoningDelta) inferencePartDelta() {}

type AudioPartDelta struct {
	Data           []byte             `json:"data"`
	Format         *media.AudioFormat `json:"format,omitempty"`
	DurationMillis *int64             `json:"duration_millis,omitempty"`
}

func (AudioPartDelta) Kind() PartKind { return PartAudio }
func (d AudioPartDelta) validateGenerateDelta() error {
	if len(d.Data) == 0 {
		return fmt.Errorf("audio delta data is required")
	}
	if d.Format != nil {
		if err := d.Format.Validate(); err != nil {
			return err
		}
	}
	if d.DurationMillis != nil && *d.DurationMillis < 0 {
		return fmt.Errorf("audio duration must not be negative")
	}
	return nil
}
func (AudioPartDelta) inferencePartDelta() {}

// ImagePartDelta carries one complete image. Images are not incrementally
// assembled by the generic runtime.
type ImagePartDelta struct {
	Part ImagePart `json:"part"`
}

func (ImagePartDelta) Kind() PartKind { return PartImage }
func (d ImagePartDelta) validateGenerateDelta() error {
	return d.Part.Validate()
}
func (ImagePartDelta) inferencePartDelta() {}

type GenerateStreamEvent struct {
	PartIndex int       `json:"part_index,omitempty"`
	Delta     PartDelta `json:"delta,omitempty"`
	// Usage is a cumulative snapshot and replaces the previous snapshot.
	Usage        *Usage       `json:"usage,omitempty"`
	FinishReason FinishReason `json:"finish_reason,omitempty"`
}

// GenerateStreamDecoder implementations must support concurrent calls.
type GenerateStreamDecoder[RawEvent any] func(
	context.Context,
	RawEvent,
) (GenerateStreamEvent, error)

type GenerateStream interface {
	Next(context.Context) (GenerateStreamEvent, error)
	Result() (GenerateResponse, error)
	Close() error
}

type ProviderStream[RawEvent any] interface {
	Next(context.Context) (RawEvent, error)
	Close() error
}

type generateStreamDriver[Wire, RawEvent any] struct {
	pipeline *pipeline[
		GenerateRequest,
		Wire,
		ProviderStream[RawEvent],
		GenerateResponse,
	]
	decode  GenerateStreamDecoder[RawEvent]
	binding *generateCompilerBinding
}

func (*generateStreamDriver[Wire, RawEvent]) inferenceGenerateStreamDriver() {}
func (d *generateStreamDriver[Wire, RawEvent]) generateCompilerBinding() *generateCompilerBinding {
	return d.binding
}

func (d *generateStreamDriver[Wire, RawEvent]) Explain(
	ctx context.Context,
	model ModelRef,
	request GenerateRequest,
) (Explanation, error) {
	return d.pipeline.explain(ctx, model, request)
}

func (d *generateStreamDriver[Wire, RawEvent]) Stream(
	ctx context.Context,
	model ModelRef,
	request GenerateRequest,
) (GenerateStream, error) {
	compiled, err := d.pipeline.prepare(ctx, model, request)
	if err != nil {
		return nil, err
	}
	raw, err := d.pipeline.transport(ctx, compiled.Wire)
	if err != nil {
		return nil, newProviderError(OperationGenerate, model.ID.Provider, err)
	}
	if isNilValue(raw) {
		return nil, NewError(
			InvalidProviderResponse,
			OperationGenerate,
			"",
			fmt.Errorf("provider opened a nil generate stream"),
		)
	}
	return &decodedGenerateStream[RawEvent]{
		raw:     raw,
		decode:  d.decode,
		model:   model,
		request: request.Clone(),
		report:  compiled.Report,
		parts:   make(map[int]*generatePartAccumulator),
	}, nil
}

type generatePartAccumulator struct {
	kind PartKind

	text strings.Builder

	toolID        string
	toolName      string
	toolArguments strings.Builder

	audio         bytes.Buffer
	audioFormat   *media.AudioFormat
	audioDuration *int64
	completeImage *ImagePart

	reasoningSignature string
	reasoningID        string
}

type decodedGenerateStream[RawEvent any] struct {
	raw     ProviderStream[RawEvent]
	decode  GenerateStreamDecoder[RawEvent]
	model   ModelRef
	request GenerateRequest
	report  CompileReport
	parts   map[int]*generatePartAccumulator
	usage   Usage
	finish  FinishReason

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
		s.resultErr = newProviderError(OperationGenerate, s.model.ID.Provider, err)
		return GenerateStreamEvent{}, s.resultErr
	}
	event, err := s.decode(ctx, rawEvent)
	if err != nil {
		s.done = true
		s.resultErr = NewError(InvalidProviderResponse, OperationGenerate, "", err)
		return GenerateStreamEvent{}, s.resultErr
	}
	if err := s.accumulate(event); err != nil {
		s.done = true
		s.resultErr = NewError(InvalidProviderResponse, OperationGenerate, "", err)
		return GenerateStreamEvent{}, s.resultErr
	}
	return event, nil
}

func (s *decodedGenerateStream[RawEvent]) Result() (GenerateResponse, error) {
	if !s.done {
		return GenerateResponse{}, NewError(
			InvalidProviderResponse,
			OperationGenerate,
			"",
			fmt.Errorf("stream is not complete"),
		)
	}
	return s.result.Clone(), s.resultErr
}

func (s *decodedGenerateStream[RawEvent]) Close() error {
	if err := s.raw.Close(); err != nil {
		return newProviderError(OperationGenerate, s.model.ID.Provider, err)
	}
	return nil
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
	if event.FinishReason != "" {
		if s.finish != "" {
			return fmt.Errorf("stream emitted multiple finish reasons")
		}
		s.finish = event.FinishReason
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
				return fmt.Errorf("tool call changed id")
			}
			p.toolID = value.ID
		}
		if value.Name != "" {
			if p.toolName != "" && p.toolName != value.Name {
				return fmt.Errorf("tool call changed name")
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
		if p.completeImage != nil {
			return fmt.Errorf("image emitted more than one complete value")
		}
		image := value.Part
		p.completeImage = &image
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
		s.resultErr = NewError(
			InvalidProviderResponse,
			OperationGenerate,
			"",
			fmt.Errorf("stream ended without a finish reason"),
		)
		return
	}
	indices := make([]int, 0, len(s.parts))
	for index := range s.parts {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	parts := make([]Part, 0, len(indices))
	for _, index := range indices {
		part, err := s.parts[index].result()
		if err != nil {
			s.resultErr = NewError(InvalidProviderResponse, OperationGenerate, "", err)
			return
		}
		parts = append(parts, part)
	}
	response := GenerateResponse{
		Message: Message{
			Role:    RoleAssistant,
			Content: Content{Parts: parts},
		},
		FinishReason: s.finish,
		Usage:        s.usage,
		Metadata:     s.report.Metadata(s.model),
	}
	deriveGenerateUsage(s.request, &response)
	if err := response.ValidateFor(s.request); err != nil {
		s.resultErr = NewError(InvalidProviderResponse, OperationGenerate, "", err)
		return
	}
	s.result = response
}

func (p *generatePartAccumulator) result() (Part, error) {
	switch p.kind {
	case PartText:
		return TextPart{Text: p.text.String()}, nil
	case PartToolCall:
		return ToolCallPart{Call: tool.Call{
			ID:        p.toolID,
			Name:      p.toolName,
			Arguments: json.RawMessage(p.toolArguments.String()),
		}}, nil
	case PartAudio:
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
		return AudioPart{
			Source:         source,
			Format:         clonePointer(p.audioFormat),
			DurationMillis: clonePointer(p.audioDuration),
		}, nil
	case PartImage:
		if p.completeImage == nil {
			return nil, fmt.Errorf("streamed image is incomplete")
		}
		return p.completeImage.Clone(), nil
	case PartReasoning:
		part := ReasoningPart{
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
