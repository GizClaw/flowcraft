package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
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
	if response.Usage.AudioDurationMillis == nil || *response.Usage.AudioDurationMillis != 2500 {
		t.Errorf("AudioDurationMillis = %v, want 2500", response.Usage.AudioDurationMillis)
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
