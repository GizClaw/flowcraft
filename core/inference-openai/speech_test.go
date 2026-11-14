package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// speechRequest is a minimal speech synthesis request.
func speechRequest(t *testing.T, text string) inference.GenerateRequest {
	t.Helper()
	request := baseRequest(t)
	request.Model.ID.Name = "tts-1"
	request.Output = inference.OutputAudio
	request.Input.Content = messageTextContent(text)
	if err := request.Validate(); err != nil {
		t.Fatalf("request: %v", err)
	}
	return request
}

// messageTextContent is the text-only content the speech surface accepts.
func messageTextContent(text string) message.Content {
	return message.Content{Parts: []message.Part{message.TextPart{Text: text}}}
}

func TestSpeechCompileProducesWire(t *testing.T) {
	speed := 1.25
	request := speechRequest(t, "hello there")
	request.Extensions = inference.Extensions{&SpeechOptions{
		Voice:        "nova",
		Format:       "wav",
		Speed:        &speed,
		Instructions: "speak slowly",
	}}
	d := &driver{settings: testSettings(APIChat)}
	compiled, err := d.speechCompile(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	wire := wireMap(t, compiled.Wire)
	if wire["input"] != "hello there" {
		t.Fatalf("input = %v", wire["input"])
	}
	if wire["model"] != "tts-1" {
		t.Fatalf("model = %v", wire["model"])
	}
	if wire["voice"] != "nova" {
		t.Fatalf("voice = %v", wire["voice"])
	}
	if wire["response_format"] != "wav" {
		t.Fatalf("response_format = %v", wire["response_format"])
	}
	if wire["instructions"] != "speak slowly" {
		t.Fatalf("instructions = %v", wire["instructions"])
	}
	for _, field := range compiled.Fields {
		if field == inference.Field("extension.openai.speech.voice") {
			return
		}
	}
	t.Fatalf("speech option field not reported: %v", compiled.Fields)
}

func TestSpeechCompileRejections(t *testing.T) {
	d := &driver{settings: testSettings(APIChat)}

	withContext := speechRequest(t, "hello")
	withContext.Context = []message.Message{{Role: message.RoleUser, Content: messageTextContent("earlier")}}
	if _, err := d.speechCompile(context.Background(), withContext, protocol.ShapeUnary); !inference.IsKind(err, inference.UnsupportedFeature) {
		t.Fatalf("context: kind = %v", err)
	}

	withImage := speechRequest(t, "hello")
	source, err := media.NewImageBytes([]byte("png"), "image/png")
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	withImage.Input.Content.Parts = append(withImage.Input.Content.Parts, message.ImagePart{Source: source})
	if _, err := d.speechCompile(context.Background(), withImage, protocol.ShapeUnary); !inference.IsKind(err, inference.UnsupportedFeature) {
		t.Fatalf("image input: kind = %v", err)
	}

	empty := speechRequest(t, "   ")
	if _, err := d.speechCompile(context.Background(), empty, protocol.ShapeUnary); !inference.IsKind(err, inference.InvalidRequest) {
		t.Fatalf("empty text: kind = %v", err)
	}

	textOutput := speechRequest(t, "hello")
	textOutput.Output = inference.OutputText
	if _, err := d.speechCompile(context.Background(), textOutput, protocol.ShapeUnary); !inference.IsKind(err, inference.UnsupportedFeature) {
		t.Fatalf("text output: kind = %v", err)
	}

	compiled, err := d.speechCompile(context.Background(), speechRequest(t, "hello"), protocol.ShapeStream)
	if err != nil {
		t.Fatalf("streaming shape rejected: %v", err)
	}
	if wireMap(t, compiled.Wire)["stream_format"] != "audio" {
		t.Fatalf("stream_format = %v, want audio", wireMap(t, compiled.Wire)["stream_format"])
	}
}

func TestSpeechDecodeWrapsAudio(t *testing.T) {
	request := speechRequest(t, "hello")
	request.Extensions = inference.Extensions{&SpeechOptions{Format: "pcm"}}
	d := &driver{settings: testSettings(APIChat)}
	response, err := d.speechDecode(context.Background(), request, []byte("RAWDATA"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("response invalid: %v", err)
	}
	if len(response.Message.Content.Parts) != 1 {
		t.Fatalf("parts = %d", len(response.Message.Content.Parts))
	}
	part, ok := response.Message.Content.Parts[0].(message.AudioPart)
	if !ok {
		t.Fatalf("part = %T", response.Message.Content.Parts[0])
	}
	if part.Source.MediaType() != "audio/pcm" {
		t.Fatalf("media type = %q", part.Source.MediaType())
	}
	if part.Format == (media.AudioFormat{}) || part.Format.Encoding != media.AudioEncodingPCM16 ||
		part.Format.SampleRateHz != 24000 || part.Format.Channels != 1 {
		t.Fatalf("format = %+v", part.Format)
	}
	// Speech is billed per character, so the counter must be reported.
	if got := response.Usage.WorkValue(usage.WorkInputCharacters); got != int64(len("hello")) {
		t.Fatalf("input characters = %d", got)
	}
	if err := response.Usage.Validate(); err != nil {
		t.Fatalf("usage invalid: %v", err)
	}
}

func TestSpeechWAVReportsNoRawEncoding(t *testing.T) {
	request := speechRequest(t, "hi")
	request.Extensions = inference.Extensions{&SpeechOptions{Format: "wav"}}
	d := &driver{settings: testSettings(APIChat)}
	response, err := d.speechDecode(context.Background(), request, []byte("WAVDATA"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	part := response.Message.Content.Parts[0].(message.AudioPart)
	if part.Source.MediaType() != "audio/wav" {
		t.Fatalf("media type = %q", part.Source.MediaType())
	}
	if part.Format != (media.AudioFormat{}) {
		t.Fatalf("wav is a container, not a raw encoding: %+v", part.Format)
	}
}

// TestSpeechConformance runs the declaration/accounting suite over the speech
// surface, which is a different pipeline from the text one.
func TestSpeechConformance(t *testing.T) {
	d := &driver{settings: testSettings(APIChat)}

	t.Run("compiler", func(t *testing.T) {
		withOptions := speechRequest(t, "hello")
		withOptions.Extensions = inference.Extensions{&SpeechOptions{Voice: "nova", Format: "mp3"}}

		inferencetest.RunCompiler(t, inferencetest.CompilerFixture[openai.AudioSpeechNewParams]{
			ProviderID: ProviderID,
			Compile: func(ctx context.Context, request inference.GenerateRequest, shape protocol.Shape) (protocol.Compiled[openai.AudioSpeechNewParams], error) {
				return d.speechCompile(ctx, request, shape)
			},
			Baseline:  speechRequest(t, "hello"),
			Supported: []inference.Field{inference.FieldInput, inference.FieldRole, inference.FieldContent, inference.FieldOutput, inference.FieldExtensions, inference.Field("extension.openai.speech.voice"), inference.Field("extension.openai.speech.format")},
			Cases: []inferencetest.FieldCase{
				{Field: inference.FieldInput, Request: speechRequest(t, "hello"), Marker: `"hello"`},
				{Field: inference.FieldRole, Request: speechRequest(t, "hello")},
				{Field: inference.FieldContent, Request: speechRequest(t, "hello"), Marker: `"input"`},
				{Field: inference.FieldOutput, Request: speechRequest(t, "hello")},
				{Field: inference.FieldExtensions, Request: withOptions, Marker: `"voice":"nova"`},
				{Field: inference.Field("extension.openai.speech.voice"), Request: withOptions, Marker: `"voice":"nova"`},
				{Field: inference.Field("extension.openai.speech.format"), Request: withOptions, Marker: `"response_format":"mp3"`},
			},
			Render: renderJSON[openai.AudioSpeechNewParams],
			Rejections: []inferencetest.Rejection{
				{Name: "conversation context", Request: withContextSpeech(t), Kind: inference.UnsupportedFeature, Field: inference.FieldContext},
				{Name: "text output", Request: baseRequest(t), Kind: inference.UnsupportedFeature, Field: inference.FieldOutput},
			},
		})
	})

	t.Run("credentialed", func(t *testing.T) {
		inferencetest.RunCredentialed(t, inferencetest.CredentialedFixture{
			EnvKey: "OPENAI_API_KEY",
			NewGenerator: func(t *testing.T) (inference.Provider, error) {
				return New(Settings{API: APIChat, APIKey: os.Getenv("OPENAI_API_KEY")})
			},
			Request: func(t *testing.T, _ inference.Output) inference.GenerateRequest {
				request := speechRequest(t, "hello there")
				request.Extensions = inference.Extensions{&SpeechOptions{Voice: "alloy", Format: "mp3"}}
				return request
			},
			Outputs: []inference.Output{inference.OutputAudio},
			Validate: func(t *testing.T, _ inference.Output, response inference.GenerateResponse) {
				part, ok := response.Message.Content.Parts[0].(message.AudioPart)
				if !ok {
					t.Fatalf("part = %T", response.Message.Content.Parts[0])
				}
				if len(part.Source.Bytes()) == 0 {
					t.Fatal("no audio bytes")
				}
				if part.Source.MediaType() != "audio/mpeg" {
					t.Fatalf("media type = %q", part.Source.MediaType())
				}
			},
		})
	})
}

func withContextSpeech(t *testing.T) inference.GenerateRequest {
	t.Helper()
	request := speechRequest(t, "hello")
	request.Context = []message.Message{{Role: message.RoleUser, Content: messageTextContent("earlier")}}
	return request
}

// TestSpeechStreamDeliversChunks walks the whole streaming path: the endpoint's
// body arrives in pieces, each piece becomes a chunk event, and the assembled
// answer holds the concatenation.
func TestSpeechStreamDeliversChunks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("AUDIO-"))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte("BYTES"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	provider, err := New(Settings{API: APIChat, APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	stream, err := provider.GenerateStream(context.Background(), speechRequest(t, "hello"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var kinds []inference.StreamEventKind
	chunks := 0
	for {
		event, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		kinds = append(kinds, event.Kind)
		if event.Kind == inference.StreamChunk {
			chunks++
			if _, ok := event.Part.(message.AudioPart); !ok {
				t.Fatalf("chunk = %T", event.Part)
			}
		}
	}
	// Two flushed writes must arrive as two chunks: that is the whole point of
	// streaming, and a single buffered read would hide a broken adapter.
	if chunks < 2 {
		t.Fatalf("chunks = %d, want at least 2: %v", chunks, kinds)
	}
	if kinds[len(kinds)-1] != inference.StreamEnd {
		t.Fatalf("last event = %q, want end", kinds[len(kinds)-1])
	}

	response, err := stream.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("assembled response invalid: %v", err)
	}
	part, ok := response.Message.Content.Parts[0].(message.AudioPart)
	if !ok {
		t.Fatalf("part = %T", response.Message.Content.Parts[0])
	}
	if string(part.Source.Bytes()) != "AUDIO-BYTES" {
		t.Fatalf("bytes = %q", part.Source.Bytes())
	}
	if part.Source.MediaType() != "audio/mpeg" {
		t.Fatalf("media type = %q", part.Source.MediaType())
	}
	if got := response.Usage.WorkValue(usage.WorkInputCharacters); got != int64(len("hello")) {
		t.Fatalf("input characters = %d", got)
	}
}

// TestSpeechStreamRejectsEmptyBody keeps an empty stream from assembling into a
// contentless response.
func TestSpeechStreamRejectsEmptyBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	provider, err := New(Settings{API: APIChat, APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	stream, err := provider.GenerateStream(context.Background(), speechRequest(t, "hello"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = stream.Close() }()
	for {
		_, err := stream.Next(context.Background())
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			t.Fatal("empty body reported a clean end")
		}
		if !inference.IsKind(err, inference.ProviderResponseUnusable) {
			t.Fatalf("kind = %v, want ProviderResponseUnusable", err)
		}
		return
	}
}
