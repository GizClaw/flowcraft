package inference

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

type RealtimeConfig struct {
	Instructions      string               `json:"instructions,omitempty" ledger:"realtime.instructions"`
	Modalities        []Modality           `json:"modalities" ledger:"realtime.modalities"`
	InputAudioFormat  *media.AudioFormat   `json:"input_audio_format,omitempty" ledger:"realtime.input_audio_format"`
	OutputAudioFormat *media.AudioFormat   `json:"output_audio_format,omitempty" ledger:"realtime.output_audio_format"`
	Voice             *media.VoiceSpec     `json:"voice,omitempty" ledger:"realtime.voice"`
	Tools             []message.Definition `json:"tools,omitempty" ledger:"realtime.tools"`
	Extensions        Extensions           `json:"-" ledger:"extension"`
}

func (c RealtimeConfig) Clone() RealtimeConfig {
	c.Modalities = append([]Modality(nil), c.Modalities...)
	c.InputAudioFormat = clonePointer(c.InputAudioFormat)
	c.OutputAudioFormat = clonePointer(c.OutputAudioFormat)
	c.Voice = clonePointer(c.Voice)
	tools := make([]message.Definition, len(c.Tools))
	for index, definition := range c.Tools {
		tools[index] = definition.Clone()
	}
	c.Tools = tools
	c.Extensions = c.Extensions.Clone()
	return c
}

func (c RealtimeConfig) Validate() error {
	if len(c.Modalities) == 0 {
		return fmt.Errorf("realtime output modalities are required")
	}
	seen := make(map[Modality]struct{}, len(c.Modalities))
	hasAudio := false
	for _, modality := range c.Modalities {
		switch modality {
		case ModalityText, ModalityAudio:
		default:
			return fmt.Errorf("unsupported realtime output modality %q", modality)
		}
		if _, ok := seen[modality]; ok {
			return fmt.Errorf("duplicate realtime output modality %q", modality)
		}
		seen[modality] = struct{}{}
		hasAudio = hasAudio || modality == ModalityAudio
	}
	if c.InputAudioFormat != nil {
		if err := c.InputAudioFormat.Validate(); err != nil {
			return fmt.Errorf("realtime input audio format: %w", err)
		}
	}
	if hasAudio {
		if c.OutputAudioFormat == nil || c.Voice == nil {
			return fmt.Errorf("audio output requires format and voice")
		}
	} else if c.OutputAudioFormat != nil || c.Voice != nil {
		return fmt.Errorf("output audio format and voice require audio output")
	}
	if c.OutputAudioFormat != nil {
		if err := c.OutputAudioFormat.Validate(); err != nil {
			return fmt.Errorf("realtime output audio format: %w", err)
		}
	}
	if c.Voice != nil {
		if err := c.Voice.Validate(); err != nil {
			return err
		}
	}
	toolNames := make(map[string]struct{}, len(c.Tools))
	for index, definition := range c.Tools {
		if err := definition.Validate(); err != nil {
			return fmt.Errorf("realtime tool %d: %w", index, err)
		}
		if _, ok := toolNames[definition.Name]; ok {
			return fmt.Errorf("duplicate realtime tool %q", definition.Name)
		}
		toolNames[definition.Name] = struct{}{}
	}
	return c.Extensions.Validate()
}

func (c RealtimeConfig) ActiveFields() []FieldID {
	var fields []FieldID
	if c.Instructions != "" {
		fields = append(fields, FieldRealtimeInstructions)
	}
	if len(c.Modalities) > 0 {
		fields = append(fields, FieldRealtimeModalities)
	}
	if c.InputAudioFormat != nil {
		fields = append(fields, FieldRealtimeInputAudioFormat)
	}
	if c.OutputAudioFormat != nil {
		fields = append(fields, FieldRealtimeOutputAudioFormat)
	}
	if c.Voice != nil {
		fields = append(fields, FieldRealtimeVoice)
	}
	if len(c.Tools) > 0 {
		fields = append(fields, FieldRealtimeTools)
	}
	return c.Extensions.AppendActiveFields(fields)
}

type RealtimeInputKind string

const (
	RealtimeInputText       RealtimeInputKind = "text"
	RealtimeInputAudio      RealtimeInputKind = "audio"
	RealtimeInputVideo      RealtimeInputKind = "video"
	RealtimeInputToolResult RealtimeInputKind = "tool_result"
)

type RealtimeInput interface {
	Kind() RealtimeInputKind
	Clone() RealtimeInput
	ActiveFields() []FieldID
	Validate() error
	inferenceRealtimeInput()
}

type RealtimeTextInput struct {
	Text string `json:"text" ledger:"realtime.input.text"`
}

func (RealtimeTextInput) Kind() RealtimeInputKind { return RealtimeInputText }
func (i RealtimeTextInput) Clone() RealtimeInput  { return i }
func (RealtimeTextInput) ActiveFields() []FieldID {
	return []FieldID{FieldRealtimeInputText}
}
func (i RealtimeTextInput) Validate() error {
	if i.Text == "" {
		return fmt.Errorf("realtime text input is required")
	}
	return nil
}
func (RealtimeTextInput) inferenceRealtimeInput() {}

type RealtimeAudioInput struct {
	Chunk media.AudioChunk `json:"chunk" ledger:"realtime.input.audio"`
}

func (RealtimeAudioInput) Kind() RealtimeInputKind { return RealtimeInputAudio }
func (i RealtimeAudioInput) Clone() RealtimeInput {
	i.Chunk = i.Chunk.Clone()
	return i
}
func (RealtimeAudioInput) ActiveFields() []FieldID {
	return []FieldID{FieldRealtimeInputAudio}
}
func (i RealtimeAudioInput) Validate() error       { return i.Chunk.Validate() }
func (RealtimeAudioInput) inferenceRealtimeInput() {}

type RealtimeVideoInput struct {
	Frame media.VideoFrame `json:"frame" ledger:"realtime.input.video"`
}

func (RealtimeVideoInput) Kind() RealtimeInputKind { return RealtimeInputVideo }
func (i RealtimeVideoInput) Clone() RealtimeInput {
	i.Frame = i.Frame.Clone()
	return i
}
func (RealtimeVideoInput) ActiveFields() []FieldID {
	return []FieldID{FieldRealtimeInputVideo}
}
func (i RealtimeVideoInput) Validate() error       { return i.Frame.Validate() }
func (RealtimeVideoInput) inferenceRealtimeInput() {}

type RealtimeToolResultInput struct {
	Result message.Result `json:"result" ledger:"realtime.input.tool_result"`
}

func (RealtimeToolResultInput) Kind() RealtimeInputKind { return RealtimeInputToolResult }
func (i RealtimeToolResultInput) Clone() RealtimeInput  { return i }
func (RealtimeToolResultInput) ActiveFields() []FieldID {
	return []FieldID{FieldRealtimeInputToolResult}
}
func (i RealtimeToolResultInput) Validate() error       { return i.Result.Validate() }
func (RealtimeToolResultInput) inferenceRealtimeInput() {}

type RealtimeEventKind string

const (
	RealtimeEventTextDelta       RealtimeEventKind = "text_delta"
	RealtimeEventAudioDelta      RealtimeEventKind = "audio_delta"
	RealtimeEventTranscriptDelta RealtimeEventKind = "transcript_delta"
	RealtimeEventToolCall        RealtimeEventKind = "tool_call"
	RealtimeEventResponseDone    RealtimeEventKind = "response_done"
)

type RealtimeEvent interface {
	Kind() RealtimeEventKind
	Clone() RealtimeEvent
	Validate() error
	inferenceRealtimeEvent()
}

type RealtimeTextDeltaEvent struct {
	Delta string `json:"delta"`
}

func (RealtimeTextDeltaEvent) Kind() RealtimeEventKind { return RealtimeEventTextDelta }
func (e RealtimeTextDeltaEvent) Clone() RealtimeEvent  { return e }
func (e RealtimeTextDeltaEvent) Validate() error {
	if e.Delta == "" {
		return fmt.Errorf("realtime text delta is required")
	}
	return nil
}
func (RealtimeTextDeltaEvent) inferenceRealtimeEvent() {}

type RealtimeAudioDeltaEvent struct {
	Chunk media.AudioChunk `json:"chunk"`
}

func (RealtimeAudioDeltaEvent) Kind() RealtimeEventKind { return RealtimeEventAudioDelta }
func (e RealtimeAudioDeltaEvent) Clone() RealtimeEvent {
	e.Chunk = e.Chunk.Clone()
	return e
}
func (e RealtimeAudioDeltaEvent) Validate() error       { return e.Chunk.Validate() }
func (RealtimeAudioDeltaEvent) inferenceRealtimeEvent() {}

type RealtimeTranscriptDeltaEvent struct {
	Delta string `json:"delta"`
}

func (RealtimeTranscriptDeltaEvent) Kind() RealtimeEventKind {
	return RealtimeEventTranscriptDelta
}
func (e RealtimeTranscriptDeltaEvent) Clone() RealtimeEvent { return e }

func (e RealtimeTranscriptDeltaEvent) Validate() error {
	if e.Delta == "" {
		return fmt.Errorf("realtime transcript delta is required")
	}
	return nil
}
func (RealtimeTranscriptDeltaEvent) inferenceRealtimeEvent() {}

type RealtimeToolCallEvent struct {
	Call message.Call `json:"call"`
}

func (RealtimeToolCallEvent) Kind() RealtimeEventKind { return RealtimeEventToolCall }
func (e RealtimeToolCallEvent) Clone() RealtimeEvent {
	e.Call = e.Call.Clone()
	return e
}
func (e RealtimeToolCallEvent) Validate() error       { return e.Call.Validate() }
func (RealtimeToolCallEvent) inferenceRealtimeEvent() {}

type RealtimeResponseDoneEvent struct {
	FinishReason FinishReason `json:"finish_reason"`
	Usage        Usage        `json:"usage"`
}

func (RealtimeResponseDoneEvent) Kind() RealtimeEventKind {
	return RealtimeEventResponseDone
}
func (e RealtimeResponseDoneEvent) Clone() RealtimeEvent {
	e.Usage = e.Usage.Clone()
	return e
}

func (e RealtimeResponseDoneEvent) Validate() error {
	switch e.FinishReason {
	case FinishCompleted, FinishMaxOutput, FinishToolCalls, FinishContentFilter,
		FinishRefusal, FinishPause, FinishInvalidToolCall, FinishContextLimit,
		FinishOther:
	default:
		return fmt.Errorf("unknown realtime finish reason %q", e.FinishReason)
	}
	return e.Usage.Validate()
}
func (RealtimeResponseDoneEvent) inferenceRealtimeEvent() {}

// RealtimeSession is a stateful bidirectional conversation. Implementations
// must allow one Send caller and one Next caller to run concurrently. Close may
// run concurrently with Next and must promptly unblock it. Canceling a Next
// context ends only that wait and must not close the session.
type RealtimeSession interface {
	Send(context.Context, RealtimeInput) error
	Next(context.Context) (RealtimeEvent, error)
	CancelResponse(context.Context) error
	Close() error
}
