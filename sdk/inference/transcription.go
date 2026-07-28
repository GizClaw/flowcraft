package inference

import (
	"context"
	"fmt"
	"math"

	"github.com/GizClaw/flowcraft/sdk/inference/media"
)

type TranscriptLanguageSource string

const (
	TranscriptLanguageRequested TranscriptLanguageSource = "requested"
	TranscriptLanguageDetected  TranscriptLanguageSource = "detected"
)

// TranscriptLanguage distinguishes a caller-supplied language echoed by a
// provider from a language actually detected from audio.
type TranscriptLanguage struct {
	Code       string                   `json:"code"`
	Source     TranscriptLanguageSource `json:"source"`
	Confidence *float64                 `json:"confidence,omitempty"`
}

func (l TranscriptLanguage) Clone() TranscriptLanguage {
	l.Confidence = clonePointer(l.Confidence)
	return l
}

func (l TranscriptLanguage) Validate() error {
	if l.Code == "" {
		return fmt.Errorf("transcript language code is required")
	}
	switch l.Source {
	case TranscriptLanguageRequested:
		if l.Confidence != nil {
			return fmt.Errorf("requested transcript language cannot have detection confidence")
		}
	case TranscriptLanguageDetected:
	default:
		return fmt.Errorf("unknown transcript language source %q", l.Source)
	}
	if l.Confidence != nil &&
		(math.IsNaN(*l.Confidence) || math.IsInf(*l.Confidence, 0) ||
			*l.Confidence < 0 || *l.Confidence > 1) {
		return fmt.Errorf("transcript language confidence must be between 0 and 1")
	}
	return nil
}

func (l TranscriptLanguage) ValidateFor(requested string) error {
	if err := l.Validate(); err != nil {
		return err
	}
	return l.validateRequested(requested)
}

func (l TranscriptLanguage) validateRequested(requested string) error {
	if l.Source == TranscriptLanguageRequested &&
		(requested == "" || l.Code != requested) {
		return fmt.Errorf("requested transcript language does not match the request")
	}
	return nil
}

type TranscriptionRequest struct {
	Audio      media.AudioSource `json:"audio" ledger:"transcription.audio"`
	Language   string            `json:"language,omitempty" ledger:"transcription.language"`
	Prompt     string            `json:"prompt,omitempty" ledger:"transcription.prompt"`
	Timestamps *bool             `json:"timestamps,omitempty" ledger:"transcription.timestamps"`
	Extensions Extensions        `json:"-" ledger:"extension"`
}

func (r TranscriptionRequest) Clone() TranscriptionRequest {
	r.Timestamps = clonePointer(r.Timestamps)
	r.Extensions = r.Extensions.Clone()
	return r
}

func (r TranscriptionRequest) ActiveFields() []FieldID {
	return r.Extensions.AppendActiveFields(activeTaggedFields(r))
}

func (r TranscriptionRequest) Validate() error {
	if err := r.Audio.Validate(); err != nil {
		return fmt.Errorf("transcription audio: %w", err)
	}
	return r.Extensions.Validate()
}

type TranscriptSegment struct {
	Text        string              `json:"text"`
	Speaker     string              `json:"speaker,omitempty"`
	StartMillis int64               `json:"start_millis"`
	EndMillis   int64               `json:"end_millis"`
	Language    *TranscriptLanguage `json:"language,omitempty"`
}

func (s TranscriptSegment) Clone() TranscriptSegment {
	if s.Language != nil {
		language := s.Language.Clone()
		s.Language = &language
	}
	return s
}

func (s TranscriptSegment) Validate() error {
	if s.Text == "" {
		return fmt.Errorf("transcript segment text is required")
	}
	if s.StartMillis < 0 || s.EndMillis < s.StartMillis {
		return fmt.Errorf("transcript segment has invalid time range")
	}
	if s.Language != nil {
		return s.Language.Validate()
	}
	return nil
}

func (s TranscriptSegment) ValidateFor(language string) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if s.Language != nil {
		return s.Language.ValidateFor(language)
	}
	return nil
}

type TranscriptionUsage struct {
	AudioDurationMillis int64  `json:"audio_duration_millis"`
	InputTokens         *int64 `json:"input_tokens,omitempty"`
	OutputTokens        *int64 `json:"output_tokens,omitempty"`
}

func (u TranscriptionUsage) Clone() TranscriptionUsage {
	u.InputTokens = clonePointer(u.InputTokens)
	u.OutputTokens = clonePointer(u.OutputTokens)
	return u
}

func (u TranscriptionUsage) Validate() error {
	if u.AudioDurationMillis < 0 ||
		(u.InputTokens != nil && *u.InputTokens < 0) ||
		(u.OutputTokens != nil && *u.OutputTokens < 0) {
		return fmt.Errorf("transcription usage values must not be negative")
	}
	return nil
}

type TranscriptionResponse struct {
	Text     string              `json:"text"`
	Language *TranscriptLanguage `json:"language,omitempty"`
	Segments []TranscriptSegment `json:"segments,omitempty"`
	Usage    TranscriptionUsage  `json:"usage"`
	Metadata Metadata            `json:"metadata"`
}

func (r TranscriptionResponse) Clone() TranscriptionResponse {
	if r.Language != nil {
		language := r.Language.Clone()
		r.Language = &language
	}
	r.Segments = append([]TranscriptSegment(nil), r.Segments...)
	for index := range r.Segments {
		r.Segments[index] = r.Segments[index].Clone()
	}
	r.Usage = r.Usage.Clone()
	r.Metadata = cloneMetadata(r.Metadata)
	return r
}

func (r TranscriptionResponse) Validate() error {
	return r.validate(nil)
}

func (r TranscriptionResponse) ValidateFor(request TranscriptionRequest) error {
	return r.validate(&request.Language)
}

func (r TranscriptionResponse) validate(language *string) error {
	var languageErr error
	if r.Language != nil {
		if err := r.Language.Validate(); err != nil {
			return err
		}
		if language != nil {
			languageErr = r.Language.validateRequested(*language)
		}
	}
	var segmentLanguageErr error
	for index, segment := range r.Segments {
		if err := segment.Validate(); err != nil {
			return fmt.Errorf("transcript segment %d: %w", index, err)
		}
		if language != nil && segment.Language != nil && segmentLanguageErr == nil {
			if err := segment.Language.validateRequested(*language); err != nil {
				segmentLanguageErr = fmt.Errorf("transcript segment %d: %w", index, err)
			}
		}
	}
	if err := r.Usage.Validate(); err != nil {
		return err
	}
	if languageErr != nil {
		return languageErr
	}
	return segmentLanguageErr
}

type TranscriptionSessionConfig struct {
	InputFormat media.AudioFormat `json:"input_format" ledger:"transcription.session.input_format"`
	Language    string            `json:"language,omitempty" ledger:"transcription.language"`
	Prompt      string            `json:"prompt,omitempty" ledger:"transcription.prompt"`
	Timestamps  *bool             `json:"timestamps,omitempty" ledger:"transcription.timestamps"`
	Extensions  Extensions        `json:"-" ledger:"extension"`
}

func (c TranscriptionSessionConfig) Clone() TranscriptionSessionConfig {
	c.Timestamps = clonePointer(c.Timestamps)
	c.Extensions = c.Extensions.Clone()
	return c
}

func (c TranscriptionSessionConfig) Validate() error {
	if err := c.InputFormat.Validate(); err != nil {
		return err
	}
	return c.Extensions.Validate()
}

func (c TranscriptionSessionConfig) ActiveFields() []FieldID {
	return c.Extensions.AppendActiveFields(activeTaggedFields(c))
}

type TranscriptionEventKind string

const (
	TranscriptionPartial       TranscriptionEventKind = "partial"
	TranscriptionFinal         TranscriptionEventKind = "final"
	TranscriptionSpeechStarted TranscriptionEventKind = "speech_started"
	TranscriptionSpeechEnded   TranscriptionEventKind = "speech_ended"
	TranscriptionUsageReported TranscriptionEventKind = "usage"
)

type TranscriptionEvent interface {
	Kind() TranscriptionEventKind
	Clone() TranscriptionEvent
	Validate() error
	inferenceTranscriptionEvent()
}

type PartialTranscriptEvent struct {
	Text     string              `json:"text"`
	Language *TranscriptLanguage `json:"language,omitempty"`
}

func (PartialTranscriptEvent) Kind() TranscriptionEventKind { return TranscriptionPartial }
func (e PartialTranscriptEvent) Clone() TranscriptionEvent {
	if e.Language != nil {
		language := e.Language.Clone()
		e.Language = &language
	}
	return e
}
func (e PartialTranscriptEvent) Validate() error {
	if e.Text == "" {
		return fmt.Errorf("partial transcript text is required")
	}
	if e.Language != nil {
		return e.Language.Validate()
	}
	return nil
}

func (e PartialTranscriptEvent) ValidateFor(config TranscriptionSessionConfig) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if e.Language != nil {
		return e.Language.ValidateFor(config.Language)
	}
	return nil
}
func (PartialTranscriptEvent) inferenceTranscriptionEvent() {}

type FinalTranscriptEvent struct {
	Text     string              `json:"text"`
	Language *TranscriptLanguage `json:"language,omitempty"`
	Segments []TranscriptSegment `json:"segments,omitempty"`
}

func (FinalTranscriptEvent) Kind() TranscriptionEventKind { return TranscriptionFinal }
func (e FinalTranscriptEvent) Clone() TranscriptionEvent {
	if e.Language != nil {
		language := e.Language.Clone()
		e.Language = &language
	}
	e.Segments = append([]TranscriptSegment(nil), e.Segments...)
	for index := range e.Segments {
		e.Segments[index] = e.Segments[index].Clone()
	}
	return e
}
func (e FinalTranscriptEvent) Validate() error {
	if e.Text == "" && len(e.Segments) == 0 {
		return fmt.Errorf("final transcript content is required")
	}
	if e.Language != nil {
		if err := e.Language.Validate(); err != nil {
			return err
		}
	}
	for index, segment := range e.Segments {
		if err := segment.Validate(); err != nil {
			return fmt.Errorf("transcript segment %d: %w", index, err)
		}
	}
	return nil
}

func (e FinalTranscriptEvent) ValidateFor(config TranscriptionSessionConfig) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if e.Language != nil {
		if err := e.Language.ValidateFor(config.Language); err != nil {
			return err
		}
	}
	for index, segment := range e.Segments {
		if err := segment.ValidateFor(config.Language); err != nil {
			return fmt.Errorf("transcript segment %d: %w", index, err)
		}
	}
	return nil
}

func (FinalTranscriptEvent) inferenceTranscriptionEvent() {}

type SpeechStartedEvent struct {
	AtMillis int64 `json:"at_millis"`
}

func (SpeechStartedEvent) Kind() TranscriptionEventKind {
	return TranscriptionSpeechStarted
}
func (e SpeechStartedEvent) Clone() TranscriptionEvent { return e }

func (e SpeechStartedEvent) Validate() error {
	if e.AtMillis < 0 {
		return fmt.Errorf("speech start time must not be negative")
	}
	return nil
}
func (SpeechStartedEvent) inferenceTranscriptionEvent() {}

type SpeechEndedEvent struct {
	AtMillis int64 `json:"at_millis"`
}

func (SpeechEndedEvent) Kind() TranscriptionEventKind { return TranscriptionSpeechEnded }
func (e SpeechEndedEvent) Clone() TranscriptionEvent  { return e }
func (e SpeechEndedEvent) Validate() error {
	if e.AtMillis < 0 {
		return fmt.Errorf("speech end time must not be negative")
	}
	return nil
}
func (SpeechEndedEvent) inferenceTranscriptionEvent() {}

type TranscriptionUsageEvent struct {
	Usage TranscriptionUsage `json:"usage"`
}

func (TranscriptionUsageEvent) Kind() TranscriptionEventKind {
	return TranscriptionUsageReported
}
func (e TranscriptionUsageEvent) Clone() TranscriptionEvent {
	e.Usage = e.Usage.Clone()
	return e
}
func (e TranscriptionUsageEvent) Validate() error            { return e.Usage.Validate() }
func (TranscriptionUsageEvent) inferenceTranscriptionEvent() {}

// TranscriptionSession is a bidirectional streaming transcription operation.
// Implementations must allow one SendAudio caller and one Next caller to run
// concurrently. CloseInput sends the provider's terminal input marker.
type TranscriptionSession interface {
	SendAudio(context.Context, media.AudioChunk) error
	CloseInput(context.Context) error
	Next(context.Context) (TranscriptionEvent, error)
	Result() (TranscriptionResponse, error)
	Close() error
}

// TranscriptionCommitter is an optional executable capability for protocols
// that can finalize one utterance and continue accepting audio on the same
// session. Protocols whose finish marker ends the request do not implement it.
type TranscriptionCommitter interface {
	Commit(context.Context) error
}
