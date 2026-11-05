package inference

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

// Transcriber is the speech-to-text seam.
//
// It is a separate interface rather than a third method on [Provider] for the
// same reason [Catalog] is optional: a provider that cannot transcribe should
// not carry a stub, and a caller that needs transcription should discover the
// capability instead of discovering the error. A provider that serves the
// workload implements it; [Catalog] declarations are checked against it at
// construction, so "declared" and "callable" cannot diverge.
type Transcriber interface {
	// Transcribe turns one audio payload into text.
	Transcribe(ctx context.Context, request TranscribeRequest) (TranscribeResponse, error)
}

// TranscribeRequest is one speech-to-text call. It carries no conversation and
// no tools: the input is audio, the answer is text.
type TranscribeRequest struct {
	// Model names the provider-qualified model that should transcribe.
	Model model.ModelRef `json:"model"`
	// Audio is the payload. It uses the same carrier a generated audio turn
	// uses, so the two directions share one representation.
	Audio message.AudioPart `json:"audio"`
	// Language is an optional ISO-639-1 hint. Empty means "detect".
	Language string `json:"language,omitempty"`
	// Prompt is optional context or vocabulary for the transcriber. It is not
	// conversation: providers that accept none report it as a warning.
	Prompt string `json:"prompt,omitempty"`
	// Segments asks for timed segments alongside the text. A provider that
	// cannot produce them explains that in a warning rather than returning an
	// empty slice, which would be indistinguishable from "no speech".
	Segments bool `json:"segments,omitempty"`
	// Extensions carries provider-specific options, as on [GenerateRequest].
	Extensions Extensions `json:"-"`
}

// Validate rejects a request before any provider I/O.
func (r TranscribeRequest) Validate() error {
	if err := r.Model.Validate(); err != nil {
		return inferenceErr(InvalidRequest, model.OperationTranscription, FieldModel, err)
	}
	if err := r.Audio.Validate(); err != nil {
		return inferenceErr(InvalidRequest, model.OperationTranscription, FieldAudio, err)
	}
	if err := r.Extensions.Validate(); err != nil {
		return inferenceErr(InvalidRequest, model.OperationTranscription, FieldExtensions, err)
	}
	return nil
}

// ActiveFields names the knobs this request uses. The contract check requires a
// compilation to honour or explain every one of them.
func (r TranscribeRequest) ActiveFields() []Field {
	fields := []Field{FieldModel, FieldAudio}
	if r.Language != "" {
		fields = append(fields, FieldLanguage)
	}
	if r.Prompt != "" {
		fields = append(fields, FieldPrompt)
	}
	if r.Segments {
		fields = append(fields, FieldSegments)
	}
	fields = append(fields, r.Extensions.ActiveFields()...)
	return fields
}

// TranscribeResponse is one transcript.
type TranscribeResponse struct {
	// Text is the transcript. It may be empty only when Segments carries the
	// speech, which ValidateFor enforces.
	Text string `json:"text"`
	// Segments are the timed pieces, present only when the request asked for
	// them and the provider could produce them.
	Segments []TranscriptSegment `json:"segments,omitempty"`
	// Usage reports what the call consumed. AudioDurationMillis is the field a
	// transcription usually fills.
	Usage usage.Usage `json:"usage,omitzero"`
	// Metadata identifies the call that produced this answer.
	Metadata Metadata `json:"metadata,omitzero"`
	// Warnings reports requested options honoured only in part.
	Warnings []Warning `json:"warnings,omitempty"`
}

// TranscriptSegment is one timed piece of a transcript.
type TranscriptSegment struct {
	Start   time.Duration `json:"start,omitempty"`
	End     time.Duration `json:"end,omitempty"`
	Text    string        `json:"text"`
	Speaker string        `json:"speaker,omitempty"`
}

// ValidateFor reports whether the answer can be the answer to request.
func (r TranscribeResponse) ValidateFor(request TranscribeRequest) error {
	if r.Text == "" && len(r.Segments) == 0 {
		return inferenceErr(InvalidProviderResponse, model.OperationTranscription, FieldContent,
			errors.New("transcript carries neither text nor segments"))
	}
	if request.Segments && len(r.Segments) == 0 {
		return inferenceErr(InvalidProviderResponse, model.OperationTranscription, FieldSegments,
			errors.New("segments were requested but the answer carries none"))
	}
	if !request.Segments && len(r.Segments) > 0 {
		return inferenceErr(InvalidProviderResponse, model.OperationTranscription, FieldSegments,
			errors.New("segments were not requested but the answer carries them"))
	}
	for i, segment := range r.Segments {
		if segment.End < segment.Start {
			return inferenceErr(InvalidProviderResponse, model.OperationTranscription, FieldSegments,
				fmt.Errorf("segment %d ends before it starts", i))
		}
	}
	return nil
}
