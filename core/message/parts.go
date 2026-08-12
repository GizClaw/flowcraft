package message

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/core/message/media"
)

type PartKind string

const (
	PartText       PartKind = "text"
	PartImage      PartKind = "image"
	PartAudio      PartKind = "audio"
	PartVideo      PartKind = "video"
	PartFile       PartKind = "file"
	PartData       PartKind = "data"
	PartToolCall   PartKind = "tool_call"
	PartToolResult PartKind = "tool_result"
	PartReasoning  PartKind = "reasoning"
)

// Part is the sealed canonical content union. Each operation validates which
// kinds it accepts; for example, Embed currently accepts only text and image.
type Part interface {
	Kind() PartKind
	Clone() Part
	Validate() error
	messagePart()
}

// NormalizePart returns part in its canonical value form. Both T and *T
// satisfy Part, so values may cross runtime boundaries as either form;
// NormalizePart collapses pointers back to values and leaves value parts
// untouched. This lets callers switch on the canonical part types without
// handling pointer duplicates.
//
// It returns an error if part is nil (including a typed nil pointer) or is not
// one of the canonical part types.
func NormalizePart(part Part) (Part, error) {
	if isNilValue(part) {
		return nil, fmt.Errorf("content part is nil")
	}
	switch value := part.(type) {
	case TextPart, ImagePart, AudioPart, VideoPart, FilePart, DataPart,
		ToolCallPart, ToolResultPart, ReasoningPart:
		return value, nil
	case *TextPart:
		return *value, nil
	case *ImagePart:
		return *value, nil
	case *AudioPart:
		return *value, nil
	case *VideoPart:
		return *value, nil
	case *FilePart:
		return *value, nil
	case *DataPart:
		return *value, nil
	case *ToolCallPart:
		return *value, nil
	case *ToolResultPart:
		return *value, nil
	case *ReasoningPart:
		return *value, nil
	default:
		return nil, fmt.Errorf("unsupported content part type %T", part)
	}
}

type TextPart struct {
	Text string `json:"text"`
}

func (TextPart) Kind() PartKind  { return PartText }
func (p TextPart) Clone() Part   { return p }
func (TextPart) Validate() error { return nil }
func (TextPart) messagePart()    {}

type ImagePart struct {
	Source media.ImageSource `json:"source"`
}

func (ImagePart) Kind() PartKind    { return PartImage }
func (p ImagePart) Clone() Part     { return p }
func (p ImagePart) Validate() error { return p.Source.Validate() }
func (ImagePart) messagePart()      {}

type AudioPart struct {
	Source         media.AudioSource  `json:"source"`
	Format         *media.AudioFormat `json:"format,omitempty"`
	DurationMillis *int64             `json:"duration_millis,omitempty"`
}

func (AudioPart) Kind() PartKind { return PartAudio }
func (p AudioPart) Clone() Part {
	p.Format = clonePointer(p.Format)
	p.DurationMillis = clonePointer(p.DurationMillis)
	return p
}

func (p AudioPart) Validate() error {
	if err := p.Source.Validate(); err != nil {
		return err
	}
	if p.Format != nil {
		if err := p.Format.Validate(); err != nil {
			return err
		}
		if p.Source.MediaType() != "" &&
			p.Source.BaseMediaType() != p.Format.Encoding.MediaType() {
			return fmt.Errorf("audio source media type does not match its format")
		}
	}
	if p.DurationMillis != nil && *p.DurationMillis < 0 {
		return fmt.Errorf("audio duration must not be negative")
	}
	return nil
}
func (AudioPart) messagePart() {}

type VideoPart struct {
	Source media.VideoSource `json:"source"`
}

func (VideoPart) Kind() PartKind    { return PartVideo }
func (p VideoPart) Clone() Part     { return p }
func (p VideoPart) Validate() error { return p.Source.Validate() }
func (VideoPart) messagePart()      {}

type FilePart struct {
	URI       string `json:"uri"`
	MediaType string `json:"media_type,omitempty"`
	Name      string `json:"name,omitempty"`
}

func (FilePart) Kind() PartKind { return PartFile }
func (p FilePart) Clone() Part  { return p }
func (p FilePart) Validate() error {
	if p.URI == "" {
		return fmt.Errorf("file URI is required")
	}
	return nil
}
func (FilePart) messagePart() {}

type DataPart struct {
	MediaType string          `json:"media_type,omitempty"`
	Value     json.RawMessage `json:"value"`
}

func (DataPart) Kind() PartKind { return PartData }
func (p DataPart) Clone() Part {
	p.Value = json.RawMessage(bytes.Clone(p.Value))
	return p
}

func (p DataPart) Validate() error {
	value := bytes.TrimSpace(p.Value)
	if len(value) == 0 || value[0] != '{' || !json.Valid(value) {
		return fmt.Errorf("data part value must be a JSON object")
	}
	return nil
}
func (DataPart) messagePart() {}

type ToolCallPart struct {
	Call ToolCall `json:"call"`
}

func (ToolCallPart) Kind() PartKind { return PartToolCall }
func (p ToolCallPart) Clone() Part {
	p.Call = p.Call.Clone()
	return p
}
func (p ToolCallPart) Validate() error { return p.Call.Validate() }
func (ToolCallPart) messagePart()      {}

type ToolResultPart struct {
	Result ToolResult `json:"result"`
}

func (ToolResultPart) Kind() PartKind    { return PartToolResult }
func (p ToolResultPart) Clone() Part     { return p }
func (p ToolResultPart) Validate() error { return p.Result.Validate() }
func (ToolResultPart) messagePart()      {}

// ReasoningPart is one provider reasoning trace: the thinking a model
// produced while composing the answer. It is a trace, not a requested
// artifact — reasoning-capable models emit it whether or not the request
// set a reasoning intent, so responses may always carry it.
//
// Text holds the visible reasoning; providers that hide the content
// (Anthropic redacted_thinking, OpenAI encrypted reasoning without a
// summary) deliver an empty Text. Signature is the provider-issued opaque
// verification payload (Anthropic signature / redacted data, OpenAI
// encrypted_content): providers that sign reasoning require it verbatim
// when the part round-trips through conversation context, so consumers
// building agent loops must preserve the whole part. The convention
// Text=="" with Signature!="" therefore means "reasoning happened, content
// withheld" — it is derived, never a separate flag. ID is the
// provider-issued trace identifier (OpenAI reasoning item ids); providers
// that address traces by id require it on round-trip, and providers
// without item ids (Anthropic thinking blocks) leave it empty.
type ReasoningPart struct {
	Text      string `json:"text,omitempty"`
	Signature string `json:"signature,omitempty"`
	ID        string `json:"id,omitempty"`
}

func (ReasoningPart) Kind() PartKind { return PartReasoning }
func (p ReasoningPart) Clone() Part  { return p }
func (p ReasoningPart) Validate() error {
	if p.Text == "" && p.Signature == "" {
		return fmt.Errorf("reasoning part carries neither text nor signature")
	}
	return nil
}
func (ReasoningPart) messagePart() {}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
