// Package inference defines the provider-neutral contracts for typed
// inference operations. It intentionally has no dependency on the legacy
// model, llm, or embedding packages.
package inference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference/media"
	"github.com/GizClaw/flowcraft/sdk/tool"
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
	inferencePart()
}

type TextPart struct {
	Text string `json:"text"`
}

func (TextPart) Kind() PartKind  { return PartText }
func (p TextPart) Clone() Part   { return p }
func (TextPart) Validate() error { return nil }
func (TextPart) inferencePart()  {}

type ImagePart struct {
	Source media.ImageSource `json:"source"`
}

func (ImagePart) Kind() PartKind    { return PartImage }
func (p ImagePart) Clone() Part     { return p }
func (p ImagePart) Validate() error { return p.Source.Validate() }
func (ImagePart) inferencePart()    {}

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
func (AudioPart) inferencePart() {}

type VideoPart struct {
	Source media.VideoSource `json:"source"`
}

func (VideoPart) Kind() PartKind    { return PartVideo }
func (p VideoPart) Clone() Part     { return p }
func (p VideoPart) Validate() error { return p.Source.Validate() }
func (VideoPart) inferencePart()    {}

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
func (FilePart) inferencePart() {}

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
func (DataPart) inferencePart() {}

type ToolCallPart struct {
	Call tool.Call `json:"call"`
}

func (ToolCallPart) Kind() PartKind { return PartToolCall }
func (p ToolCallPart) Clone() Part {
	p.Call = p.Call.Clone()
	return p
}
func (p ToolCallPart) Validate() error { return p.Call.Validate() }
func (ToolCallPart) inferencePart()    {}

type ToolResultPart struct {
	Result tool.Result `json:"result"`
}

func (ToolResultPart) Kind() PartKind    { return PartToolResult }
func (p ToolResultPart) Clone() Part     { return p }
func (p ToolResultPart) Validate() error { return p.Result.Validate() }
func (ToolResultPart) inferencePart()    {}

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
func (ReasoningPart) inferencePart() {}

// Content is an ordered collection of canonical parts. Intent deliberately
// does not belong here; only InputContent may attach execution intent.
type Content struct {
	Parts []Part `json:"parts"`
}

func (c Content) Clone() Content {
	if c.Parts == nil {
		return Content{}
	}
	cloned := Content{Parts: make([]Part, len(c.Parts))}
	for i, part := range c.Parts {
		if part != nil {
			cloned.Parts[i] = part.Clone()
		}
	}
	return cloned
}

func (c Content) Validate() error {
	if len(c.Parts) == 0 {
		return fmt.Errorf("content must contain at least one part")
	}
	for i, part := range c.Parts {
		switch part.(type) {
		case TextPart, ImagePart, AudioPart, VideoPart, FilePart, DataPart,
			ToolCallPart, ToolResultPart, ReasoningPart:
		default:
			return fmt.Errorf("content part %d has unsupported value type %T", i, part)
		}
		if err := part.Validate(); err != nil {
			return fmt.Errorf("content part %d: %w", i, err)
		}
	}
	return nil
}

func (c Content) MarshalJSON() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	wire := make([]any, len(c.Parts))
	for i, part := range c.Parts {
		switch value := part.(type) {
		case TextPart:
			wire[i] = struct {
				Type PartKind `json:"type"`
				Text string   `json:"text"`
			}{PartText, value.Text}
		case ImagePart:
			wire[i] = struct {
				Type   PartKind          `json:"type"`
				Source media.ImageSource `json:"source"`
			}{PartImage, value.Source}
		case AudioPart:
			wire[i] = struct {
				Type           PartKind           `json:"type"`
				Source         media.AudioSource  `json:"source"`
				Format         *media.AudioFormat `json:"format,omitempty"`
				DurationMillis *int64             `json:"duration_millis,omitempty"`
			}{PartAudio, value.Source, value.Format, value.DurationMillis}
		case VideoPart:
			wire[i] = struct {
				Type   PartKind          `json:"type"`
				Source media.VideoSource `json:"source"`
			}{PartVideo, value.Source}
		case FilePart:
			wire[i] = struct {
				Type      PartKind `json:"type"`
				URI       string   `json:"uri"`
				MediaType string   `json:"media_type,omitempty"`
				Name      string   `json:"name,omitempty"`
			}{PartFile, value.URI, value.MediaType, value.Name}
		case DataPart:
			wire[i] = struct {
				Type      PartKind        `json:"type"`
				MediaType string          `json:"media_type,omitempty"`
				Value     json.RawMessage `json:"value"`
			}{PartData, value.MediaType, value.Value}
		case ToolCallPart:
			wire[i] = struct {
				Type PartKind  `json:"type"`
				Call tool.Call `json:"call"`
			}{PartToolCall, value.Call}
		case ToolResultPart:
			wire[i] = struct {
				Type   PartKind    `json:"type"`
				Result tool.Result `json:"result"`
			}{PartToolResult, value.Result}
		case ReasoningPart:
			wire[i] = struct {
				Type      PartKind `json:"type"`
				Text      string   `json:"text,omitempty"`
				Signature string   `json:"signature,omitempty"`
				ID        string   `json:"id,omitempty"`
			}{PartReasoning, value.Text, value.Signature, value.ID}
		default:
			return nil, fmt.Errorf("unsupported content part %T", part)
		}
	}
	return json.Marshal(struct {
		Parts []any `json:"parts"`
	}{Parts: wire})
}

func (c *Content) UnmarshalJSON(data []byte) error {
	var wire struct {
		Parts []json.RawMessage `json:"parts"`
	}
	if err := decodeStrict(data, &wire); err != nil {
		return err
	}
	decoded := Content{Parts: make([]Part, len(wire.Parts))}
	for i, raw := range wire.Parts {
		var header struct {
			Type PartKind `json:"type"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return fmt.Errorf("content part %d: %w", i, err)
		}
		switch header.Type {
		case PartText:
			var item struct {
				Type PartKind `json:"type"`
				Text *string  `json:"text"`
			}
			if err := decodeStrict(raw, &item); err != nil {
				return fmt.Errorf("content part %d: %w", i, err)
			}
			if item.Text == nil {
				return fmt.Errorf("content part %d has no text payload", i)
			}
			decoded.Parts[i] = TextPart{Text: *item.Text}
		case PartImage:
			var item struct {
				Type   PartKind          `json:"type"`
				Source media.ImageSource `json:"source"`
			}
			if err := decodeStrict(raw, &item); err != nil {
				return fmt.Errorf("content part %d: %w", i, err)
			}
			decoded.Parts[i] = ImagePart{Source: item.Source}
		case PartAudio:
			var item struct {
				Type           PartKind           `json:"type"`
				Source         media.AudioSource  `json:"source"`
				Format         *media.AudioFormat `json:"format,omitempty"`
				DurationMillis *int64             `json:"duration_millis,omitempty"`
			}
			if err := decodeStrict(raw, &item); err != nil {
				return fmt.Errorf("content part %d: %w", i, err)
			}
			decoded.Parts[i] = AudioPart{
				Source: item.Source, Format: item.Format,
				DurationMillis: item.DurationMillis,
			}
		case PartVideo:
			var item struct {
				Type   PartKind          `json:"type"`
				Source media.VideoSource `json:"source"`
			}
			if err := decodeStrict(raw, &item); err != nil {
				return fmt.Errorf("content part %d: %w", i, err)
			}
			decoded.Parts[i] = VideoPart{Source: item.Source}
		case PartFile:
			var item struct {
				Type      PartKind `json:"type"`
				URI       string   `json:"uri"`
				MediaType string   `json:"media_type,omitempty"`
				Name      string   `json:"name,omitempty"`
			}
			if err := decodeStrict(raw, &item); err != nil {
				return fmt.Errorf("content part %d: %w", i, err)
			}
			decoded.Parts[i] = FilePart{
				URI: item.URI, MediaType: item.MediaType, Name: item.Name,
			}
		case PartData:
			var item struct {
				Type      PartKind        `json:"type"`
				MediaType string          `json:"media_type,omitempty"`
				Value     json.RawMessage `json:"value"`
			}
			if err := decodeStrict(raw, &item); err != nil {
				return fmt.Errorf("content part %d: %w", i, err)
			}
			decoded.Parts[i] = DataPart{MediaType: item.MediaType, Value: item.Value}
		case PartToolCall:
			var item struct {
				Type PartKind  `json:"type"`
				Call tool.Call `json:"call"`
			}
			if err := decodeStrict(raw, &item); err != nil {
				return fmt.Errorf("content part %d: %w", i, err)
			}
			decoded.Parts[i] = ToolCallPart{Call: item.Call}
		case PartToolResult:
			var item struct {
				Type   PartKind    `json:"type"`
				Result tool.Result `json:"result"`
			}
			if err := decodeStrict(raw, &item); err != nil {
				return fmt.Errorf("content part %d: %w", i, err)
			}
			decoded.Parts[i] = ToolResultPart{Result: item.Result}
		case PartReasoning:
			var item struct {
				Type      PartKind `json:"type"`
				Text      string   `json:"text,omitempty"`
				Signature string   `json:"signature,omitempty"`
				ID        string   `json:"id,omitempty"`
			}
			if err := decodeStrict(raw, &item); err != nil {
				return fmt.Errorf("content part %d: %w", i, err)
			}
			decoded.Parts[i] = ReasoningPart{
				Text:      item.Text,
				Signature: item.Signature,
				ID:        item.ID,
			}
		default:
			return fmt.Errorf("content part %d has unknown type %q", i, header.Type)
		}
	}
	if err := decoded.Validate(); err != nil {
		return err
	}
	*c = decoded
	return nil
}

func decodeStrict(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

// Text concatenates the message's text parts in order. Non-text parts
// are skipped; it answers "what did the user/assistant say" without
// imposing prose structure on multi-modal content.
func (c Content) Text() string {
	var b strings.Builder
	for _, part := range c.Parts {
		if tp, ok := part.(TextPart); ok {
			b.WriteString(tp.Text)
		}
	}
	return b.String()
}
