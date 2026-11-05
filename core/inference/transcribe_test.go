package inference_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

func validTranscribe() inference.TranscribeRequest {
	return inference.TranscribeRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "gpt-4o-mini-transcribe"}},
		Audio: message.AudioPart{},
	}
}

func TestTranscribeRequestValidate(t *testing.T) {
	base := validTranscribe()
	// A zero audio part has no source, so the base request must already fail:
	// that is the check a caller hits first.
	if err := base.Validate(); err == nil {
		t.Error("Validate(no audio) = nil, want an error")
	}

	missingModel := validTranscribe()
	missingModel.Model = model.ModelRef{}
	if err := missingModel.Validate(); err == nil {
		t.Error("Validate(no model) = nil, want an error")
	}
}

func TestTranscribeActiveFields(t *testing.T) {
	base := validTranscribe()
	got := base.ActiveFields()
	if len(got) != 2 || got[0] != inference.FieldModel || got[1] != inference.FieldAudio {
		t.Fatalf("ActiveFields(minimal) = %v, want [model audio]", got)
	}

	full := validTranscribe()
	full.Language = "en"
	full.Prompt = "names: FlowCraft"
	full.Segments = true
	got = full.ActiveFields()
	for _, want := range []inference.Field{
		inference.FieldModel, inference.FieldAudio,
		inference.FieldLanguage, inference.FieldPrompt, inference.FieldSegments,
	} {
		found := false
		for _, field := range got {
			if field == want {
				found = true
			}
		}
		if !found {
			t.Errorf("ActiveFields misses %q: %v", want, got)
		}
	}
}

func TestTranscribeResponseValidateFor(t *testing.T) {
	request := validTranscribe()

	if err := (inference.TranscribeResponse{}).ValidateFor(request); err == nil {
		t.Error("empty transcript accepted, want an error")
	}
	if err := (inference.TranscribeResponse{Text: "hi"}).ValidateFor(request); err != nil {
		t.Errorf("text-only answer rejected: %v", err)
	}

	asking := request
	asking.Segments = true
	if err := (inference.TranscribeResponse{Text: "hi"}).ValidateFor(asking); err == nil {
		t.Error("segments requested but absent, want an error")
	}
	segments := inference.TranscribeResponse{Segments: []inference.TranscriptSegment{{Text: "hi"}}}
	if err := segments.ValidateFor(asking); err != nil {
		t.Errorf("segments answer rejected: %v", err)
	}
	if err := segments.ValidateFor(request); err == nil {
		t.Error("unrequested segments accepted, want an error")
	}

	backwards := inference.TranscribeResponse{Segments: []inference.TranscriptSegment{
		{Start: 2e9, End: 1e9, Text: "x"},
	}}
	if err := backwards.ValidateFor(asking); err == nil {
		t.Error("segment ending before it starts accepted, want an error")
	}
}
