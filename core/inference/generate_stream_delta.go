package inference

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

// PartDelta is the sealed provider-neutral union accepted by GenerateStream.
type PartDelta interface {
	Kind() message.PartKind
	validateGenerateDelta() error
	inferencePartDelta()
}

func normalizePartDelta(delta PartDelta) (PartDelta, error) {
	if ptr.IsNil(delta) {
		return nil, fmt.Errorf("generate part delta is nil")
	}
	switch value := delta.(type) {
	case TextPartDelta, ToolCallDelta, ReasoningDelta, AudioPartDelta, ImagePartDelta:
		return value, nil
	case *TextPartDelta:
		return *value, nil
	case *ToolCallDelta:
		return *value, nil
	case *ReasoningDelta:
		return *value, nil
	case *AudioPartDelta:
		return *value, nil
	case *ImagePartDelta:
		return *value, nil
	default:
		return nil, fmt.Errorf("unsupported generate part delta %T", delta)
	}
}

type TextPartDelta struct {
	Text string `json:"text"`
}

func (TextPartDelta) Kind() message.PartKind       { return message.PartText }
func (TextPartDelta) validateGenerateDelta() error { return nil }
func (TextPartDelta) inferencePartDelta()          {}

// ToolCallDelta carries provider-neutral incremental tool-call arguments.
// ArgumentsFragment is validated only after stream accumulation.
type ToolCallDelta struct {
	ID                string `json:"id,omitempty"`
	Name              string `json:"name,omitempty"`
	ArgumentsFragment string `json:"arguments_fragment,omitempty"`
}

func (ToolCallDelta) Kind() message.PartKind       { return message.PartToolCall }
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

func (ReasoningDelta) Kind() message.PartKind { return message.PartReasoning }
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

func (AudioPartDelta) Kind() message.PartKind { return message.PartAudio }
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
// assembled by the generic runtime. A delta marked Interim is a progress
// snapshot: it replaces any image previously accumulated at the same part
// index, and the terminal image for that index (a delta without Interim)
// replaces the last snapshot. Interim deltas surface to stream consumers as
// progress, while the final result keeps only the last image per index.
type ImagePartDelta struct {
	Part message.ImagePart `json:"part"`
	// Interim marks the delta as a progress snapshot that replaces any
	// previously accumulated image at the same part index. A part index
	// must end with a non-Interim delta to be included in the result.
	Interim bool `json:"interim,omitempty"`
}

func (ImagePartDelta) Kind() message.PartKind { return message.PartImage }
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
	// FinishSynthesized marks a finish reason the adapter fabricated
	// because the provider ended the stream without emitting a terminal
	// finish event (for example a chat stream that closed cleanly between
	// frames). It is false for provider-emitted finish reasons. A
	// synthesized finish may mean the response was truncated mid-flight,
	// so consumers can treat the result with reduced trust.
	FinishSynthesized bool `json:"finish_synthesized,omitempty"`
	// ProviderOutputs carries a cumulative snapshot per provider output
	// family (citations, search-call status). An entry with the same
	// provider/extension identity replaces the previous snapshot, matching
	// Usage; the terminal result carries the final collection.
	ProviderOutputs ProviderOutputs `json:"provider_outputs,omitempty"`
	// RequestID / ResponseID ride the terminal finish event when the
	// provider exposes them. The stream accumulator carries them onto
	// the final Result metadata.
	RequestID  string `json:"request_id,omitempty"`
	ResponseID string `json:"response_id,omitempty"`
}
