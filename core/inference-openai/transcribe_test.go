package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// transcriptionRequest builds a text-out request whose input is audio. That is
// the whole transcription signal: there is no operation field to set.
func transcriptionRequest(t *testing.T, options *TranscriptionOptions) inference.GenerateRequest {
	t.Helper()
	source, err := media.NewAudioBytes([]byte("fake-audio"), "audio/mpeg")
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	request := baseRequest(t)
	request.Model.ID.Name = "whisper-1"
	request.Output = inference.OutputText
	request.Input.Content = message.Content{Parts: []message.Part{
		message.AudioPart{Source: source},
	}}
	if options != nil {
		request.Extensions = inference.Extensions{options}
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("request: %v", err)
	}
	return request
}

func TestTranscriptionDecodesText(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"hello there","duration":2.5,"language":"en"}`)
	}))
	defer server.Close()

	provider, err := New(Settings{API: APIChat, APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	response, err := provider.Generate(context.Background(), transcriptionRequest(t, nil))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if path != "/audio/transcriptions" && path != "/v1/audio/transcriptions" {
		t.Errorf("called %q, want the transcription endpoint", path)
	}
	if got := response.Message.Content.Text(); got != "hello there" {
		t.Errorf("text = %q, want %q", got, "hello there")
	}
	if got := response.Usage.WorkValue(usage.WorkAudioDurationMillis); got != 2500 {
		t.Errorf("audio duration = %d, want 2500", got)
	}
}

func TestTranscriptionSegmentsRideADataPart(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The SDK decodes into its own segment type, whose fields are
		// required; a real provider answer carries them.
		_, _ = io.WriteString(w, `{"text":"a b","duration":1.5,"language":"en",
			"segments":[
				{"id":0,"start":0.0,"end":0.75,"text":"a","avg_logprob":-0.1,"compression_ratio":1.0,"no_speech_prob":0.0,"temperature":0.0,"tokens":[1]},
				{"id":1,"start":0.75,"end":1.5,"text":"b","avg_logprob":-0.1,"compression_ratio":1.0,"no_speech_prob":0.0,"temperature":0.0,"tokens":[2]}]}`)
	}))
	defer server.Close()

	provider, err := New(Settings{API: APIChat, APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	response, err := provider.Generate(context.Background(),
		transcriptionRequest(t, &TranscriptionOptions{Segments: true}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	var found bool
	for _, part := range response.Message.Content.Parts {
		data, ok := part.(message.DataPart)
		if !ok {
			continue
		}
		if data.MediaType != TranscriptMediaType {
			t.Errorf("data part media type = %q, want %q", data.MediaType, TranscriptMediaType)
		}
		found = true
	}
	if !found {
		t.Errorf("no data part carrying segments: %+v", response.Message.Content.Parts)
	}
}

// A model that returns no segments must say so: an empty slice would be
// indistinguishable from silence.
func TestTranscriptionWarnsWhenSegmentsMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"hello","duration":1.0}`)
	}))
	defer server.Close()

	provider, err := New(Settings{API: APIChat, APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	response, err := provider.Generate(context.Background(),
		transcriptionRequest(t, &TranscriptionOptions{Segments: true}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(response.Warnings) == 0 {
		t.Fatal("segments were requested but the answer carries no warning")
	}
}

// A text-only declaration must reject audio before any provider I/O.
func TestTranscriptionRejectedByDeclaredInputs(t *testing.T) {
	declared := model.ModelDescriptor{
		ID:    model.ModelID{Provider: ProviderID, Name: "gpt-5-mini"},
		Label: "text only",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText},
			Outputs: []message.PartKind{message.PartText},
		},
	}
	provider, err := New(Settings{
		API: APIChat, APIKey: "test-key", BaseURL: "http://127.0.0.1:1",
		Models: []model.ModelDescriptor{declared},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	request := transcriptionRequest(t, nil)
	request.Model.ID.Name = "gpt-5-mini"
	if _, err := provider.Generate(context.Background(), request); err == nil {
		t.Fatal("audio sent to a text-only model was accepted")
	}
}

// A declared byte ceiling must reject the payload locally, naming the limit,
// instead of uploading it and letting the provider answer with a 413.
func TestTranscriptionEnforcesMaxInputBytes(t *testing.T) {
	ceiling := int64(4)
	declared := model.ModelDescriptor{
		ID:    model.ModelID{Provider: ProviderID, Name: "whisper-1"},
		Label: "small budget",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartAudio},
			Outputs: []message.PartKind{message.PartText},
		},
		Limits: model.ModelLimits{MaxInputBytes: &ceiling},
	}
	provider, err := New(Settings{
		API: APIChat, APIKey: "test-key", BaseURL: "http://127.0.0.1:1",
		Models: []model.ModelDescriptor{declared},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	_, err = provider.Generate(context.Background(), transcriptionRequest(t, nil))
	if err == nil {
		t.Fatal("oversized audio accepted, want a local rejection")
	}
	if !inference.IsKind(err, inference.InvalidRequest) {
		t.Errorf("kind = %v, want InvalidRequest", err)
	}
	// Error() renders kind and field only; the numbers live in the wrapped
	// cause, which is where a caller looks when it wants the detail.
	cause := errors.Unwrap(err)
	if cause == nil || !strings.Contains(cause.Error(), "at most 4") {
		t.Errorf("wrapped cause %v does not name the limit", cause)
	}
}

// The multipart encoder names the file part from Filename(); without it the
// endpoint rejects the upload as an unnamed blob. This pins both the mapping and
// the refusal to guess an unknown container.
func TestTranscriptionFilenameForAudio(t *testing.T) {
	for mediaType, want := range map[string]string{
		"audio/mpeg": "audio.mp3",
		"audio/wav":  "audio.wav",
		"audio/webm": "audio.webm",
		"audio/m4a":  "audio.m4a",
		"audio/flac": "audio.flac",
		"audio/ogg":  "audio.ogg",
	} {
		got, err := filenameForAudio(mediaType)
		if err != nil || got != want {
			t.Errorf("filenameForAudio(%q) = %q, %v; want %q, nil", mediaType, got, err, want)
		}
	}
	if _, err := filenameForAudio("audio/aiff"); err == nil {
		t.Error("an unlisted container was accepted; a wrong extension is a wrong format")
	}
}
