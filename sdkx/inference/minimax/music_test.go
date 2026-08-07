package minimax

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

func musicTestRequest(prompt string) inference.GenerateRequest {
	return inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{
					Parts: []message.Part{message.TextPart{Text: prompt}},
				},
				Intent: inference.Intent{Audio: &inference.AudioIntent{
					Format: media.AudioFormat{Encoding: media.AudioEncodingMP3},
				}},
			},
		},
	}
}

func musicOKServer(t *testing.T, audio []byte) *messagesServer {
	t.Helper()
	return newMessagesServer(t, func(w http.ResponseWriter, body map[string]any) {
		payload, _ := json.Marshal(map[string]any{
			"data":      map[string]any{"audio": hex.EncodeToString(audio), "status": 2},
			"base_resp": map[string]any{"status_code": 0, "status_msg": "success"},
		})
		_, _ = w.Write(payload)
	})
}

func rejectField(
	t *testing.T,
	runtime *inference.Runtime,
	model inference.ModelRef,
	request inference.GenerateRequest,
	wantKind inference.ErrorKind,
	wantField inference.FieldID,
) *inference.Error {
	t.Helper()
	_, err := runtime.Generate(context.Background(), model, request)
	if err == nil {
		t.Fatal("expected rejection")
	}
	if !inference.IsKind(err, wantKind) {
		t.Fatalf("err = %v, want kind %v", err, wantKind)
	}
	var failure *inference.Error
	if !errors.As(err, &failure) {
		t.Fatalf("err = %v, not an *inference.Error", err)
	}
	if wantField != "" && failure.Field != wantField {
		t.Fatalf("field = %q, want %q", failure.Field, wantField)
	}
	return failure
}

func TestMusicUnary(t *testing.T) {
	audio := []byte("fake-mp3-bytes")
	server := musicOKServer(t, audio)
	runtime := newTestRuntime(t, server)

	instrumental := false
	watermark := true
	request := musicTestRequest("独立民谣, 忧郁")
	request.Input.Content.Intent.Audio.Format = media.AudioFormat{
		Encoding:     media.AudioEncodingPCM16,
		SampleRateHz: 44100,
		Channels:     2,
	}
	request.Extensions = inference.Extensions{MusicOptions{
		Lyrics:       "[verse]\n街灯微亮",
		Instrumental: &instrumental,
		Watermark:    &watermark,
	}}
	response, err := runtime.Generate(context.Background(), minimaxModel("music-3.0"), request)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	part, ok := response.Message.Content.Parts[0].(message.AudioPart)
	if !ok {
		t.Fatalf("part type = %T", response.Message.Content.Parts[0])
	}
	if string(part.Source.Bytes()) != string(audio) {
		t.Fatalf("audio bytes = %q", part.Source.Bytes())
	}
	if part.Format == nil || part.Format.Encoding != media.AudioEncodingPCM16 {
		t.Fatalf("format = %+v", part.Format)
	}
	if response.FinishReason != inference.FinishCompleted {
		t.Fatalf("finish = %v", response.FinishReason)
	}

	sent := server.body(t, 0)
	if sent["model"] != "music-3.0" {
		t.Fatalf("model = %v", sent["model"])
	}
	if sent["prompt"] != "独立民谣, 忧郁" {
		t.Fatalf("prompt = %v", sent["prompt"])
	}
	if sent["lyrics"] != "[verse]\n街灯微亮" {
		t.Fatalf("lyrics = %v", sent["lyrics"])
	}
	if value, ok := sent["is_instrumental"].(bool); !ok || value {
		t.Fatalf("is_instrumental = %v", sent["is_instrumental"])
	}
	if sent["aigc_watermark"] != true {
		t.Fatalf("aigc_watermark = %v", sent["aigc_watermark"])
	}
	setting, _ := sent["audio_setting"].(map[string]any)
	if setting["format"] != "pcm" || setting["sample_rate"] != float64(44100) {
		t.Fatalf("audio_setting = %v", setting)
	}
}

func TestMusicRejectsVoice(t *testing.T) {
	server := musicOKServer(t, []byte("x"))
	runtime := newTestRuntime(t, server)

	request := musicTestRequest("pop")
	request.Input.Content.Intent.Audio.Voice = media.VoiceSpec{ID: "male-qn-qingse"}
	_ = rejectField(t, runtime, minimaxModel("music-3.0"), request,
		inference.UnsupportedFeature, inference.FieldGenerateIntentAudioVoiceID)
	if server.requests() != 0 {
		t.Fatalf("transport ran %d times", server.requests())
	}
}

func TestMusicRejectsSpeedAndChannels(t *testing.T) {
	server := musicOKServer(t, []byte("x"))
	runtime := newTestRuntime(t, server)

	speed := 1.5
	request := musicTestRequest("pop")
	request.Input.Content.Intent.Audio.Speed = &speed
	_ = rejectField(t, runtime, minimaxModel("music-3.0"), request,
		inference.UnsupportedFeature, inference.FieldGenerateIntentAudioSpeed)

	request = musicTestRequest("pop")
	request.Input.Content.Intent.Audio.Format = media.AudioFormat{
		Encoding:     media.AudioEncodingPCM16,
		SampleRateHz: 44100,
		Channels:     1,
	}
	_ = rejectField(t, runtime, minimaxModel("music-3.0"), request,
		inference.UnsupportedFeature, inference.FieldGenerateIntentAudioFormatChannels)
	if server.requests() != 0 {
		t.Fatalf("transport ran %d times", server.requests())
	}
}

func TestMusicRejectsForeignExtension(t *testing.T) {
	server := newMessagesServer(t, func(w http.ResponseWriter, body map[string]any) {
		t.Errorf("transport should not run")
	})
	runtime := newTestRuntime(t, server)

	// MusicOptions on a speech model rejects as InvalidExtension. The
	// request carries a voice so the extension rejection surfaces first.
	request := musicTestRequest("pop")
	request.Input.Content.Intent.Audio.Voice = media.VoiceSpec{ID: "male-qn-qingse"}
	request.Extensions = inference.Extensions{MusicOptions{Lyrics: "la"}}
	_ = rejectField(t, runtime, minimaxModel("speech-2.8-hd"), request,
		inference.InvalidExtension, "")
}

func TestMusicRejectsStreamWatermark(t *testing.T) {
	server := musicOKServer(t, []byte("x"))
	runtime := newTestRuntime(t, server)

	watermark := true
	request := musicTestRequest("pop")
	request.Extensions = inference.Extensions{MusicOptions{Watermark: &watermark}}
	_, err := runtime.GenerateStream(context.Background(), minimaxModel("music-3.0"), request)
	if err == nil {
		t.Fatal("expected rejection")
	}
	if !inference.IsKind(err, inference.InvalidExtension) {
		t.Fatalf("err = %v, want InvalidExtension", err)
	}
}

func TestMusicStream(t *testing.T) {
	chunks := []string{"aa", "bb"}
	server := newMessagesServer(t, func(w http.ResponseWriter, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range chunks {
			payload, _ := json.Marshal(map[string]any{
				"data":      map[string]any{"audio": chunk, "status": 1},
				"base_resp": map[string]any{"status_code": 0},
			})
			_, _ = w.Write([]byte("data: " + string(payload) + "\n\n"))
		}
		payload, _ := json.Marshal(map[string]any{
			"data":      map[string]any{"audio": "", "status": 2},
			"base_resp": map[string]any{"status_code": 0},
		})
		_, _ = w.Write([]byte("data: " + string(payload) + "\n\n"))
	})
	runtime := newTestRuntime(t, server)

	stream, err := runtime.GenerateStream(context.Background(), minimaxModel("music-3.0"), musicTestRequest("jazz"))
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	var deltas int
	finished := false
	for {
		event, err := stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if _, ok := event.Delta.(inference.AudioPartDelta); ok {
			deltas++
		}
		if event.FinishReason == inference.FinishCompleted {
			finished = true
		}
	}
	if deltas != len(chunks) {
		t.Fatalf("deltas = %d, want %d", deltas, len(chunks))
	}
	if !finished {
		t.Fatal("no finish event")
	}

	sent := server.body(t, 0)
	if sent["stream"] != true || sent["output_format"] != "hex" {
		t.Fatalf("stream flags = %v %v", sent["stream"], sent["output_format"])
	}
}
