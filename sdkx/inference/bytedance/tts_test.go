package bytedance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
	"github.com/GizClaw/flowcraft/sdkx/inference/config"
)

// unwrapLog prints the full error chain for debugging classification failures.
func unwrapLog(t *testing.T, err error) {
	t.Helper()
	for depth := 0; err != nil; depth++ {
		t.Logf("depth %d: %T: %v", depth, err, err)
		err = errors.Unwrap(err)
	}
}

// ttsChunkLine renders one line of the TTS V2 HTTP chunk protocol.
func ttsChunkLine(code int, audio []byte) string {
	data := "null"
	if audio != nil {
		encoded := base64.StdEncoding.EncodeToString(audio)
		data = `"` + encoded + `"`
	}
	return fmt.Sprintf(`{"reqid":"req-1","code":%d,"message":"","data":%s}`, code, data) + "\n"
}

func newSpeechRuntime(
	t *testing.T,
	server *httptest.Server,
) *inference.Runtime {
	t.Helper()
	spec, err := json.Marshal(map[string]any{
		"speech_base_url": server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := config.NewSecret([]byte("test-key"))
	if err != nil {
		t.Fatal(err)
	}
	profileSpec, err := json.Marshal(ProfileSpec{AppID: "test-app"})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := Factory().Build(context.Background(), config.ProviderInput{
		ID:   "bytedance",
		Spec: spec,
		Profiles: []config.ResolvedProfile{{
			ID:      "default",
			Secrets: map[string]config.Secret{SecretAPIKey: key},
			Spec:    profileSpec,
		}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	runtime, err := inference.NewRuntime([]inference.ProviderDefinition{provider})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return runtime
}

func ttsGenerateRequest(format media.AudioFormat) inference.GenerateRequest {
	return inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{
					Parts: []message.Part{message.TextPart{Text: "read this aloud"}},
				},
				Intent: inference.Intent{
					Audio: &inference.AudioIntent{
						Voice:  media.VoiceSpec{ID: "zh_female_qingxin"},
						Format: format,
					},
				},
			},
		},
	}
}

func TestTTSCapturedWire(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, ttsChunkLine(0, []byte("audio-one")))
		flusher.Flush()
		fmt.Fprint(w, ttsChunkLine(0, []byte("audio-two")))
		flusher.Flush()
		fmt.Fprint(w, ttsChunkLine(20000000, nil))
		flusher.Flush()
	}))
	defer server.Close()
	runtime := newSpeechRuntime(t, server)

	response, err := runtime.Generate(
		context.Background(),
		generateModel("doubao-tts-2-0"),
		ttsGenerateRequest(media.AudioFormat{
			Encoding:     media.AudioEncodingMP3,
			SampleRateHz: 24000,
		}),
	)
	if err != nil {
		for depth, current := 0, err; current != nil && depth < 8; depth++ {
			t.Logf("depth %d: %T: %v", depth, current, current)
			current = errors.Unwrap(current)
		}
		t.Fatalf("Generate: %v", err)
	}
	if len(response.Message.Content.Parts) != 1 {
		t.Fatalf("parts = %d", len(response.Message.Content.Parts))
	}
	part, ok := response.Message.Content.Parts[0].(message.AudioPart)
	if !ok {
		t.Fatalf("part = %#v", response.Message.Content.Parts[0])
	}
	if string(part.Source.Bytes()) != "audio-oneaudio-two" {
		t.Fatalf("audio = %q", part.Source.Bytes())
	}
	if part.Format == nil || part.Format.Encoding != media.AudioEncodingMP3 ||
		part.Format.SampleRateHz != 24000 {
		t.Fatalf("format = %+v", part.Format)
	}

	request, _ := captured["req_params"].(map[string]any)
	if request == nil {
		t.Fatalf("captured body = %v", captured)
	}
	if request["text"] != "read this aloud" {
		t.Fatalf("text = %v", request["text"])
	}
	if request["speaker"] != "zh_female_qingxin" {
		t.Fatalf("speaker = %v", request["speaker"])
	}
	audio, _ := request["audio_params"].(map[string]any)
	if audio["format"] != "mp3" || audio["sample_rate"].(float64) != 24000 {
		t.Fatalf("audio_params = %v", audio)
	}
}

func TestTTSStreamDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, ttsChunkLine(0, []byte("chunk-1")))
		flusher.Flush()
		fmt.Fprint(w, ttsChunkLine(0, []byte("chunk-2")))
		flusher.Flush()
		fmt.Fprint(w, ttsChunkLine(20000000, nil))
		flusher.Flush()
	}))
	defer server.Close()
	runtime := newSpeechRuntime(t, server)

	stream, err := runtime.GenerateStream(
		context.Background(),
		generateModel("doubao-tts-2-0"),
		ttsGenerateRequest(media.AudioFormat{
			Encoding:     media.AudioEncodingPCM16,
			SampleRateHz: 16000,
			Channels:     1,
		}),
	)
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer stream.Close()

	var deltas int
	var sawFinish bool
	for {
		event, err := stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			unwrapLog(t, err)
			t.Fatalf("Next: %v", err)
		}
		if delta, ok := event.Delta.(inference.AudioPartDelta); ok {
			deltas++
			if delta.Format == nil || delta.Format.Encoding != media.AudioEncodingPCM16 {
				t.Fatalf("delta format = %+v", delta.Format)
			}
		}
		if event.FinishReason != "" {
			sawFinish = true
		}
	}
	result, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if deltas != 2 {
		t.Fatalf("deltas = %d", deltas)
	}
	if !sawFinish {
		t.Fatal("no finish event")
	}
	part := result.Message.Content.Parts[0].(message.AudioPart)
	if string(part.Source.Bytes()) != "chunk-1chunk-2" {
		t.Fatalf("audio = %q", part.Source.Bytes())
	}
}

func TestTTSRejections(t *testing.T) {
	speed := 3.0
	count := 2
	cases := []struct {
		name   string
		mutate func(*inference.GenerateRequest)
		field  inference.FieldID
	}{
		{
			name: "speed outside provider range",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Audio.Speed = &speed
			},
			field: inference.FieldGenerateIntentAudioSpeed,
		},
		{
			name: "count above one",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Audio.Count = &count
			},
			field: inference.FieldGenerateIntentAudioCount,
		},
		{
			name: "unsupported encoding",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Audio.Format = media.AudioFormat{
					Encoding:     media.AudioEncodingFloat32,
					SampleRateHz: 24000,
					Channels:     1,
				}
			},
			field: inference.FieldGenerateIntentAudioFormatEncoding,
		},
		{
			name: "non-image part in content",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Parts = append(r.Input.Content.Parts,
					message.DataPart{MediaType: "application/vnd.x", Value: json.RawMessage(`{}`)})
			},
			field: inference.FieldGenerateInputData,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("transport must not run after compiler rejection")
			}))
			defer server.Close()
			runtime := newSpeechRuntime(t, server)
			request := ttsGenerateRequest(media.AudioFormat{Encoding: media.AudioEncodingMP3})
			tc.mutate(&request)
			_, err := runtime.Generate(
				context.Background(),
				generateModel("doubao-tts-2-0"),
				request,
			)
			if err == nil {
				t.Fatal("expected compiler rejection")
			}
			var inferenceErr *inference.Error
			if !errors.As(err, &inferenceErr) || inferenceErr.Field != tc.field {
				t.Fatalf("field = %v, want %s", err, tc.field)
			}
			if !inference.IsKind(err, inference.UnsupportedFeature) {
				t.Fatalf("kind = %v", err)
			}
		})
	}
}
