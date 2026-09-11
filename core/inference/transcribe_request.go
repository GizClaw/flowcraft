package inference

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/message/media"
)

// TranscriptionRequest is the canonical input for whole-file speech
// recognition. The audio source is mandatory; language, prompt, and
// timestamps are optional provider-neutral controls whose support is decided
// per driver at compile time through the field ledger.
type TranscriptionRequest struct {
	Audio      media.AudioSource `json:"audio"`
	Language   string            `json:"language,omitempty"`
	Prompt     string            `json:"prompt,omitempty"`
	Timestamps bool              `json:"timestamps,omitempty"`
	Extensions Extensions        `json:"-" ledger:"extension"`
}

func (r TranscriptionRequest) Clone() TranscriptionRequest {
	r.Audio = r.Audio.Clone()
	r.Extensions = r.Extensions.Clone()
	return r
}

func (r TranscriptionRequest) Validate() error {
	if err := r.Audio.Validate(); err != nil {
		return err
	}
	if r.Audio.Kind() == media.SourceStream {
		return fmt.Errorf(
			"transcription request audio must be complete: " +
				"use TranscribeSession for stream sources",
		)
	}
	return r.Extensions.Validate()
}

func (r TranscriptionRequest) ActiveFields() []FieldID {
	fields := []FieldID{FieldTranscriptionAudio}
	if r.Language != "" {
		fields = append(fields, FieldTranscriptionLanguage)
	}
	if r.Prompt != "" {
		fields = append(fields, FieldTranscriptionPrompt)
	}
	if r.Timestamps {
		fields = append(fields, FieldTranscriptionTimestamps)
	}
	return r.Extensions.AppendActiveFields(fields)
}

// TranscriptionSegment is one completed utterance of a transcript.
// StartMillis/EndMillis bound the utterance in the session's audio timeline
// when the provider reports timing; Words carries word-level timing when
// available.
type TranscriptionSegment struct {
	Text        string              `json:"text"`
	StartMillis int64               `json:"start_millis,omitempty"`
	EndMillis   int64               `json:"end_millis,omitempty"`
	Words       []TranscriptionWord `json:"words,omitempty"`
}

func (s TranscriptionSegment) Validate() error {
	if s.Text == "" {
		return fmt.Errorf("transcription segment text is required")
	}
	if s.StartMillis < 0 || s.EndMillis < 0 {
		return fmt.Errorf("transcription segment timestamps must not be negative")
	}
	if s.StartMillis != 0 && s.EndMillis != 0 && s.EndMillis < s.StartMillis {
		return fmt.Errorf("transcription segment end precedes start")
	}
	for index, word := range s.Words {
		if err := word.Validate(); err != nil {
			return fmt.Errorf("transcription segment word %d: %w", index, err)
		}
	}
	return nil
}

type TranscriptionWord struct {
	Word        string `json:"word"`
	StartMillis int64  `json:"start_millis,omitempty"`
	EndMillis   int64  `json:"end_millis,omitempty"`
}

func (w TranscriptionWord) Validate() error {
	if w.Word == "" {
		return fmt.Errorf("transcription word text is required")
	}
	if w.StartMillis < 0 || w.EndMillis < 0 {
		return fmt.Errorf("transcription word timestamps must not be negative")
	}
	if w.StartMillis != 0 && w.EndMillis != 0 && w.EndMillis < w.StartMillis {
		return fmt.Errorf("transcription word end precedes start")
	}
	return nil
}

// TranscriptionResponse is the canonical recognition result. Text is the
// joined transcript; Segments carries per-utterance text with optional
// timing; Language reports the request hint or the detected language;
// DurationMillis is the recognized audio duration when the provider reports
// one.
type TranscriptionResponse struct {
	Text           string                 `json:"text"`
	Segments       []TranscriptionSegment `json:"segments,omitempty"`
	Language       string                 `json:"language,omitempty"`
	DurationMillis *int64                 `json:"duration_millis,omitempty"`
	Usage          Usage                  `json:"usage"`
	Metadata       Metadata               `json:"metadata"`
}

func (r TranscriptionResponse) Clone() TranscriptionResponse {
	r.Segments = append([]TranscriptionSegment(nil), r.Segments...)
	r.Usage = r.Usage.Clone()
	r.Metadata = r.Metadata.Clone()
	return r
}

func (r TranscriptionResponse) ValidateFor(request TranscriptionRequest) error {
	if err := validateTranscriptionSegments(r.Segments); err != nil {
		return err
	}
	if r.DurationMillis != nil && *r.DurationMillis < 0 {
		return fmt.Errorf("transcription duration must not be negative")
	}
	return nil
}

func validateTranscriptionSegments(segments []TranscriptionSegment) error {
	for index, segment := range segments {
		if err := segment.Validate(); err != nil {
			return fmt.Errorf("transcription segment %d: %w", index, err)
		}
	}
	return nil
}

// TranscriptionSessionRequest opens a duplex speech-recognition session.
// Audio arrives incrementally after open through TranscriptionSession.Send;
// InputFormat is therefore mandatory and negotiated at open time. Language,
// Prompt, and Timestamps follow the unary semantics.
type TranscriptionSessionRequest struct {
	InputFormat media.AudioFormat `json:"input_format"`
	Language    string            `json:"language,omitempty"`
	Prompt      string            `json:"prompt,omitempty"`
	Timestamps  bool              `json:"timestamps,omitempty"`
	Extensions  Extensions        `json:"-" ledger:"extension"`
}

func (r TranscriptionSessionRequest) Clone() TranscriptionSessionRequest {
	r.Extensions = r.Extensions.Clone()
	return r
}

func (r TranscriptionSessionRequest) Validate() error {
	if err := r.InputFormat.Validate(); err != nil {
		return fmt.Errorf("transcription session input format: %w", err)
	}
	return r.Extensions.Validate()
}

func (r TranscriptionSessionRequest) ActiveFields() []FieldID {
	fields := []FieldID{FieldTranscriptionInputFormat}
	if r.Language != "" {
		fields = append(fields, FieldTranscriptionLanguage)
	}
	if r.Prompt != "" {
		fields = append(fields, FieldTranscriptionPrompt)
	}
	if r.Timestamps {
		fields = append(fields, FieldTranscriptionTimestamps)
	}
	return r.Extensions.AppendActiveFields(fields)
}

// TranscriptionSessionEvent is one canonical session event. Text carries the
// full current utterance hypothesis (partial); Final closes the current
// utterance and commits it to the transcript; Segment is an alternative
// provider style that commits a completed utterance directly. Providers
// choose one style per event, never both.
type TranscriptionSessionEvent struct {
	Text        string                `json:"text,omitempty"`
	Final       bool                  `json:"final,omitempty"`
	Segment     *TranscriptionSegment `json:"segment,omitempty"`
	StartMillis int64                 `json:"start_millis,omitempty"`
	EndMillis   int64                 `json:"end_millis,omitempty"`
	Language    string                `json:"language,omitempty"`
	// Usage is a cumulative session snapshot and replaces the previous
	// snapshot.
	Usage      *Usage `json:"usage,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	ResponseID string `json:"response_id,omitempty"`
}

func (e TranscriptionSessionEvent) empty() bool {
	return e.Text == "" && !e.Final && e.Segment == nil &&
		e.StartMillis == 0 && e.EndMillis == 0 && e.Language == "" &&
		e.Usage == nil && e.RequestID == "" && e.ResponseID == ""
}
