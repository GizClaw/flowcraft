package minimax

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

// Speech synthesis pipeline tests, run end to end through the runtime
// against the fake server: the t2a_v2 wire shape, the compiler's
// capability gate, base_resp error classification, and the SSE stream.

// ttsGenerateRequest builds a speech request: one user text part and an
// audio intent naming the voice; the format carries the encoding knobs.
func ttsGenerateRequest(format media.AudioFormat) inference.GenerateRequest {
	return inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{
					Parts: []message.Part{message.TextPart{Text: "hello minimax"}},
				},
				Intent: inference.Intent{
					Audio: &inference.AudioIntent{
						Voice:  media.VoiceSpec{ID: "male-qn-qingse"},
						Format: format,
					},
				},
			},
		},
	}
}

// ttsUnaryBody renders the unary t2a_v2 envelope with hex-encoded audio.
func ttsUnaryBody(audio string) string {
	payload, _ := json.Marshal(map[string]any{
		"data":      map[string]any{"audio": hex.EncodeToString([]byte(audio))},
		"base_resp": map[string]any{"status_code": 0, "status_msg": "success"},
	})
	return string(payload)
}

// ttsSSEBody renders one SSE document per event. Media SSE carries bare
// data lines — no event headers like the Messages stream.
func ttsSSEBody(events ...map[string]any) string {
	body := ""
	for _, event := range events {
		payload, _ := json.Marshal(event)
		body += "data: " + string(payload) + "\n\n"
	}
	return body
}

// ttsStreamChunk renders one t2a_v2 stream event: status 1 carries an
// audio chunk, status 2 terminates the stream.
func ttsStreamChunk(status int, audio string) map[string]any {
	return map[string]any{
		"data": map[string]any{
			"audio":  hex.EncodeToString([]byte(audio)),
			"status": status,
		},
		"base_resp": map[string]any{"status_code": 0, "status_msg": "success"},
	}
}

func TestTTSUnaryCapturedWire(t *testing.T) {
	server := newMessagesServer(t, func(w http.ResponseWriter, _ map[string]any) {
		fmt.Fprint(w, ttsUnaryBody("spoken-audio"))
	})
	runtime := newTestRuntime(t, server)

	speed := 1.2
	request := ttsGenerateRequest(media.AudioFormat{
		Encoding:     media.AudioEncodingMP3,
		SampleRateHz: 24000,
		Channels:     1,
	})
	request.Input.Content.Intent.Audio.Speed = &speed
	response, err := runtime.Generate(context.Background(), minimaxModel("speech-2.8-hd"), request)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.FinishReason != inference.FinishCompleted {
		t.Fatalf("finish = %q", response.FinishReason)
	}
	if len(response.Message.Content.Parts) != 1 {
		t.Fatalf("parts = %d", len(response.Message.Content.Parts))
	}
	part, ok := response.Message.Content.Parts[0].(message.AudioPart)
	if !ok {
		t.Fatalf("part = %#v", response.Message.Content.Parts[0])
	}
	if string(part.Source.Bytes()) != "spoken-audio" {
		t.Fatalf("audio = %q", part.Source.Bytes())
	}
	if part.Format == nil || part.Format.Encoding != media.AudioEncodingMP3 ||
		part.Format.SampleRateHz != 24000 || part.Format.Channels != 1 {
		t.Fatalf("format = %+v", part.Format)
	}
	if server.requests() != 1 {
		t.Fatalf("requests = %d", server.requests())
	}

	body := server.body(t, 0)
	if body["model"] != "speech-2.8-hd" {
		t.Fatalf("model = %v", body["model"])
	}
	if body["text"] != "hello minimax" {
		t.Fatalf("text = %v", body["text"])
	}
	if body["stream"] != false || body["output_format"] != "hex" {
		t.Fatalf("stream/output_format = %v/%v", body["stream"], body["output_format"])
	}
	voice, _ := body["voice_setting"].(map[string]any)
	if voice["voice_id"] != "male-qn-qingse" || voice["speed"] != speed {
		t.Fatalf("voice_setting = %v", voice)
	}
	audio, _ := body["audio_setting"].(map[string]any)
	if audio["format"] != "mp3" || audio["sample_rate"] != float64(24000) ||
		audio["channel"] != float64(1) {
		t.Fatalf("audio_setting = %v", audio)
	}
	if body["language_boost"] != "auto" {
		t.Fatalf("language_boost = %v", body["language_boost"])
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
			name: "pcm24 encoding",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Audio.Format = media.AudioFormat{
					Encoding:     media.AudioEncodingPCM24,
					SampleRateHz: 24000,
					Channels:     1,
				}
			},
			field: inference.FieldGenerateIntentAudioFormatEncoding,
		},
		{
			name: "count above one",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Audio.Count = &count
			},
			field: inference.FieldGenerateIntentAudioCount,
		},
		{
			name: "unsupported sample rate",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Audio.Format = media.AudioFormat{
					Encoding:     media.AudioEncodingMP3,
					SampleRateHz: 48000,
				}
			},
			field: inference.FieldGenerateIntentAudioFormatSampleRate,
		},
		{
			name: "assistant context role",
			mutate: func(r *inference.GenerateRequest) {
				r.Context = []message.Message{{
					Role: message.RoleAssistant,
					Content: message.Content{
						Parts: []message.Part{message.TextPart{Text: "prior turn"}},
					},
				}}
			},
			field: inference.FieldGenerateContextRole,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newMessagesServer(t, func(w http.ResponseWriter, _ map[string]any) {
				t.Error("transport must not run after compiler rejection")
			})
			runtime := newTestRuntime(t, server)
			request := ttsGenerateRequest(media.AudioFormat{Encoding: media.AudioEncodingMP3})
			tc.mutate(&request)
			_, err := runtime.Generate(
				context.Background(),
				minimaxModel("speech-2.8-hd"),
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
			if server.requests() != 0 {
				t.Fatalf("transport ran %d times", server.requests())
			}
		})
	}
}

// TestTTSBaseRespClassification asserts the base_resp status code drives
// the errdefs taxonomy through the runtime's provider-failure wrapping.
func TestTTSBaseRespClassification(t *testing.T) {
	cases := []struct {
		name  string
		code  int
		check func(error) bool
	}{
		{"rate limited", statusRateLimited, errdefs.IsRateLimit},
		{"auth failed", statusAuthFailed, errdefs.IsUnauthorized},
		{"invalid key", statusInvalidKey, errdefs.IsUnauthorized},
		{"no balance", statusNoBalance, errdefs.IsForbidden},
		{"sensitive content", statusSensitive, errdefs.IsValidation},
		{"bad params", statusBadParams, errdefs.IsValidation},
		{"unknown code", 9999, errdefs.IsNotAvailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newMessagesServer(t, func(w http.ResponseWriter, _ map[string]any) {
				payload, _ := json.Marshal(map[string]any{
					"data":      map[string]any{"audio": ""},
					"base_resp": map[string]any{"status_code": tc.code, "status_msg": "boom"},
				})
				fmt.Fprint(w, string(payload))
			})
			runtime := newTestRuntime(t, server)
			_, err := runtime.Generate(
				context.Background(),
				minimaxModel("speech-2.8-hd"),
				ttsGenerateRequest(media.AudioFormat{Encoding: media.AudioEncodingMP3}),
			)
			if err == nil {
				t.Fatal("expected base_resp failure")
			}
			if !tc.check(err) {
				t.Fatalf("classification = %v", err)
			}
			if !inference.IsKind(err, inference.ProviderFailure) {
				t.Fatalf("kind = %v, want provider failure", err)
			}
		})
	}
}

func TestTTSStreamDeltas(t *testing.T) {
	server := newMessagesServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ttsSSEBody(
			ttsStreamChunk(1, "chunk-1"),
			ttsStreamChunk(1, "chunk-2"),
			ttsStreamChunk(2, ""),
		))
	})
	runtime := newTestRuntime(t, server)

	stream, err := runtime.GenerateStream(
		context.Background(),
		minimaxModel("speech-2.8-hd"),
		ttsGenerateRequest(media.AudioFormat{
			Encoding:     media.AudioEncodingFLAC,
			SampleRateHz: 16000,
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
			t.Fatalf("Next: %v", err)
		}
		if delta, ok := event.Delta.(inference.AudioPartDelta); ok {
			deltas++
			if delta.Format == nil || delta.Format.Encoding != media.AudioEncodingFLAC ||
				delta.Format.SampleRateHz != 16000 {
				t.Fatalf("delta format = %+v", delta.Format)
			}
		}
		if event.FinishReason != "" {
			sawFinish = true
		}
	}
	if deltas != 2 {
		t.Fatalf("deltas = %d", deltas)
	}
	if !sawFinish {
		t.Fatal("no finish event")
	}
	result, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if len(result.Message.Content.Parts) != 1 {
		t.Fatalf("parts = %d", len(result.Message.Content.Parts))
	}
	part, ok := result.Message.Content.Parts[0].(message.AudioPart)
	if !ok {
		t.Fatalf("part = %#v", result.Message.Content.Parts[0])
	}
	if string(part.Source.Bytes()) != "chunk-1chunk-2" {
		t.Fatalf("audio = %q", part.Source.Bytes())
	}

	body := server.body(t, 0)
	if body["stream"] != true {
		t.Fatalf("stream = %v", body["stream"])
	}
	audio, _ := body["audio_setting"].(map[string]any)
	if audio["format"] != "flac" || audio["sample_rate"] != float64(16000) {
		t.Fatalf("audio_setting = %v", audio)
	}
}

// TestTTSStreamBaseRespError asserts a failing base_resp mid-stream
// surfaces on Next with its errdefs classification intact.
func TestTTSStreamBaseRespError(t *testing.T) {
	server := newMessagesServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ttsSSEBody(map[string]any{
			"data":      map[string]any{"audio": "", "status": 1},
			"base_resp": map[string]any{"status_code": statusRateLimited, "status_msg": "slow down"},
		}))
	})
	runtime := newTestRuntime(t, server)

	stream, err := runtime.GenerateStream(
		context.Background(),
		minimaxModel("speech-2.8-hd"),
		ttsGenerateRequest(media.AudioFormat{Encoding: media.AudioEncodingMP3}),
	)
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer stream.Close()

	_, err = stream.Next(context.Background())
	if err == nil || err == io.EOF {
		t.Fatalf("Next = %v, want classified failure", err)
	}
	if !errdefs.IsRateLimit(err) {
		t.Fatalf("classification = %v", err)
	}
}
