package dashscope

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// speechRequest builds one speech request whose input carries the text and the
// audio intent under test. An intent that names no format gets the one the
// platform's canonical validation requires — the Qwen-TTS family's served
// shape — so a case that means to test the format asks names them itself.
func speechRequest(text string, audio *inference.AudioIntent) inference.GenerateRequest {
	if audio != nil && (audio.Format == media.AudioFormat{}) {
		audio.Format = media.AudioFormat{
			Encoding:     media.AudioEncodingPCM16,
			SampleRateHz: 24000,
			Channels:     1,
		}
	}
	return userRequest(
		[]message.Part{message.TextPart{Text: text}},
		inference.Intent{Audio: audio},
	)
}

// TestCompileTTSPinsSurface pins the Qwen-TTS lowering: the prompt is the
// request text, the voice rides input.voice, and the language rides
// input.language_type only when the declaration serves the control.
func TestCompileTTSPinsSurface(t *testing.T) {
	declared := testDeclaration(t, "qwen3-tts-flash")
	request := speechRequest("今天天气真不错。", &inference.AudioIntent{
		Voice: media.VoiceSpec{ID: "Cherry", Language: "Chinese"},
		Format: media.AudioFormat{
			Encoding:     media.AudioEncodingPCM16,
			SampleRateHz: 24000,
			Channels:     1,
		},
	})
	request.Context = []message.Message{{
		Role: message.RoleUser,
		Content: message.Content{
			Parts: []message.Part{message.TextPart{Text: "读得慢一点"}},
		},
	}}

	wire, report, err := compileTTSWire(
		t, declared, request, inference.GenerateExecutionUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if wire.model != "qwen3-tts-flash" {
		t.Errorf("model = %q", wire.model)
	}
	if wire.text != "读得慢一点\n今天天气真不错。" {
		t.Errorf("text = %q", wire.text)
	}
	if wire.voice != "Cherry" {
		t.Errorf("voice = %q", wire.voice)
	}
	if wire.language != "Chinese" {
		t.Errorf("language = %q", wire.language)
	}
	for _, field := range []inference.FieldID{
		inference.FieldGenerateIntentAudio,
		inference.FieldGenerateIntentAudioVoice,
		inference.FieldGenerateIntentAudioVoiceID,
		inference.FieldGenerateIntentAudioVoiceLanguage,
		inference.FieldGenerateIntentAudioFormatEncoding,
		inference.FieldGenerateIntentAudioFormatSampleRate,
		inference.FieldGenerateIntentAudioFormatChannels,
	} {
		decision, ok := decisionFor(report, field)
		if !ok || decision.Disposition != inference.Native {
			t.Errorf("%s decision = %+v", field, decision)
		}
	}
	body := ttsRequest(wire)
	input, ok := body["input"].(map[string]any)
	if !ok {
		t.Fatalf("input = %#v", body["input"])
	}
	if input["text"] != wire.text || input["voice"] != "Cherry" ||
		input["language_type"] != "Chinese" {
		t.Errorf("input = %#v", input)
	}
	if _, ok := body["parameters"]; ok {
		t.Errorf("the Qwen-TTS surface takes no parameters block: %#v", body)
	}
}

// TestCompileTTSRefusesControls pins the fail-closed half: every control the
// Qwen-TTS surface does not carry is refused on its own field, and a
// non-streaming surface refuses a streaming request.
func TestCompileTTSRefusesControls(t *testing.T) {
	declared := testDeclaration(t, "qwen3-tts-flash")
	voice := media.VoiceSpec{ID: "Cherry"}
	for _, tc := range []struct {
		name    string
		audio   *inference.AudioIntent
		request func(inference.GenerateRequest) inference.GenerateRequest
		shape   inference.GenerateExecutionShape
		field   inference.FieldID
		reason  string
	}{
		{
			name:   "no audio intent",
			audio:  nil,
			field:  inference.FieldGenerateInputRole,
			reason: "requires an audio intent",
		},
		{
			name:   "no voice",
			audio:  &inference.AudioIntent{},
			field:  inference.FieldGenerateIntentAudioVoice,
			reason: "requires a voice",
		},
		{
			name: "requested encoding the model does not serve",
			audio: &inference.AudioIntent{
				Voice:  voice,
				Format: media.AudioFormat{Encoding: media.AudioEncodingMP3},
			},
			field:  inference.FieldGenerateIntentAudioFormatEncoding,
			reason: "no encoding control",
		},
		{
			name: "requested sample rate the model does not serve",
			audio: &inference.AudioIntent{
				Voice:  voice,
				Format: media.AudioFormat{SampleRateHz: 48000},
			},
			field:  inference.FieldGenerateIntentAudioFormatSampleRate,
			reason: "serves 24000 Hz",
		},
		{
			name: "requested stereo",
			audio: &inference.AudioIntent{
				Voice:  voice,
				Format: media.AudioFormat{Channels: 2},
			},
			field:  inference.FieldGenerateIntentAudioFormatChannels,
			reason: "serves 1 channel",
		},
		{
			name: "requested speed",
			audio: &inference.AudioIntent{
				Voice: voice,
				Speed: floatPtr(1.5),
			},
			field:  inference.FieldGenerateIntentAudioSpeed,
			reason: "no speed control",
		},
		{
			name: "requested count",
			audio: &inference.AudioIntent{
				Voice: voice,
				Count: intPtr(2),
			},
			field:  inference.FieldGenerateIntentAudioCount,
			reason: "single audio stream",
		},
		{
			name: "text intent",
			audio: &inference.AudioIntent{
				Voice: voice,
			},
			request: func(request inference.GenerateRequest) inference.GenerateRequest {
				request.Input.Content.Intent.Text = &inference.TextIntent{
					MaxOutputTokens: intPtr(32),
				}
				return request
			},
			field:  inference.FieldGenerateIntentText,
			reason: "do not produce text",
		},
		{
			name:   "streaming request",
			audio:  &inference.AudioIntent{Voice: voice},
			shape:  inference.GenerateExecutionStream,
			field:  inference.FieldGenerateExecutionStream,
			reason: "unary on this provider",
		},
		{
			name:  "prompt over the declared cap",
			audio: &inference.AudioIntent{Voice: voice},
			request: func(request inference.GenerateRequest) inference.GenerateRequest {
				request.Input.Content.Parts = []message.Part{
					message.TextPart{Text: strings.Repeat("字", 601)},
				}
				return request
			},
			field:  inference.FieldGenerateInputText,
			reason: "caps the text at 600 characters",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shape := tc.shape
			if shape == "" {
				shape = inference.GenerateExecutionUnary
			}
			request := speechRequest("你好。", tc.audio)
			if tc.request != nil {
				request = tc.request(request)
			}
			wire, report, err := compileTTSWire(t, declared, request, shape)
			if err == nil {
				t.Fatalf("compile accepted the request: %+v", wire)
			}
			if got := rejectedReason(report, tc.field); !strings.Contains(got, tc.reason) {
				t.Errorf("reason for %s = %q, want %q", tc.field, got, tc.reason)
			}
		})
	}
}

// TestCompileTTSRefusesLanguageWithoutDeclaration pins the declaration gate:
// the language control rides input.language_type only where the deployment
// declared it, so a model without the fact refuses the ask instead of
// dropping it.
func TestCompileTTSRefusesLanguageWithoutDeclaration(t *testing.T) {
	declared := testDeclaration(t, "qwen3-tts-flash")
	declared.TTS.LanguageSelect = false
	request := speechRequest("你好。", &inference.AudioIntent{
		Voice: media.VoiceSpec{ID: "Cherry", Language: "Chinese"},
	})
	_, report, err := compileTTSWire(
		t, declared, request, inference.GenerateExecutionUnary)
	if err == nil {
		t.Fatal("compile accepted a language on a model without the control")
	}
	reason := rejectedReason(report, inference.FieldGenerateIntentAudioVoiceLanguage)
	if !strings.Contains(reason, "language_select") {
		t.Errorf("reason = %q", reason)
	}
}

// TestCompileTTSRefusesUndeclaredFormat pins the declaration gate on the
// format asks: a model whose declaration names no served shape refuses a named
// one on its own field, so the caller learns which declaration fact is missing
// instead of receiving a shape nobody checked.
func TestCompileTTSRefusesUndeclaredFormat(t *testing.T) {
	declared := testDeclaration(t, "qwen3-tts-flash")
	declared.TTS.Encoding = ""
	declared.TTS.SampleRateHz = 0
	declared.TTS.Channels = 0
	request := speechRequest("你好。", &inference.AudioIntent{
		Voice: media.VoiceSpec{ID: "Cherry"},
	})
	_, report, err := compileTTSWire(
		t, declared, request, inference.GenerateExecutionUnary)
	if err == nil {
		t.Fatal("compile accepted a format on a declaration that names none")
	}
	for field, want := range map[inference.FieldID]string{
		inference.FieldGenerateIntentAudioFormatEncoding:   "set tts.encoding",
		inference.FieldGenerateIntentAudioFormatSampleRate: "set tts.sample_rate_hz",
		inference.FieldGenerateIntentAudioFormatChannels:   "set tts.channels",
	} {
		if got := rejectedReason(report, field); !strings.Contains(got, want) {
			t.Errorf("reason for %s = %q, want %q", field, got, want)
		}
	}
}

// TestCompileTTSRefusesMediaAndExtensions pins the surface's input channel:
// speech synthesis speaks text, so an image part and a driver extension are
// refused rather than flattened into the prompt or ignored.
func TestCompileTTSRefusesMediaAndExtensions(t *testing.T) {
	declared := testDeclaration(t, "qwen3-tts-flash")
	request := speechRequest("你好。", &inference.AudioIntent{
		Voice: media.VoiceSpec{ID: "Cherry"},
	})
	request.Input.Content.Parts = append(
		request.Input.Content.Parts,
		imagePart(t, "https://example.invalid/a.png"),
	)
	request.Extensions = inference.Extensions{GenerateOptions{
		Provider: providerID,
		Seed:     int64Ptr(7),
	}}
	_, report, err := compileTTSWire(
		t, declared, request, inference.GenerateExecutionUnary)
	if err == nil {
		t.Fatal("compile accepted an image part and an extension")
	}
	if got := rejectedReason(report, inference.FieldGenerateInputImage); !strings.Contains(got, "not image") {
		t.Errorf("image reason = %q", got)
	}
	if got := rejectedReason(report, generateOptionsField("seed")); !strings.Contains(got, "speech synthesis") {
		t.Errorf("extension reason = %q", got)
	}
}

// TestTransportTTSPostsSurface drives the wire call: the model and the input
// block, nothing else, and the answer's URL becomes one audio part whose media
// type is the container the platform serves.
func TestTransportTTSPostsSurface(t *testing.T) {
	platform := &fakePlatform{}
	platform.sync = func(body map[string]any) (int, string) {
		if body["model"] != "qwen3-tts-flash" {
			t.Errorf("model = %v", body["model"])
		}
		if _, ok := body["parameters"]; ok {
			t.Errorf("the surface carried a parameters block: %#v", body)
		}
		input, ok := body["input"].(map[string]any)
		if !ok {
			t.Fatalf("input = %#v", body["input"])
		}
		if raw, _ := json.Marshal(input); string(raw) !=
			`{"language_type":"Chinese","text":"你好。","voice":"Cherry"}` {
			t.Errorf("input = %s", raw)
		}
		return 200, `{"status_code":200,"request_id":"req-tts-1","output":{` +
			`"finish_reason":"stop","audio":{"data":"","id":"audio_1",` +
			`"url":"https://dashscope-result.oss-cn-beijing.aliyuncs.com/a.wav?Expires=1",` +
			`"expires_at":1766113409}},` +
			`"usage":{"input_tokens":0,"output_tokens":0,"characters":12}}`
	}
	declared := testDeclaration(t, "qwen3-tts-flash")
	operations, err := openTTS(
		&clients{media: testClients(t, platform)}, declared,
		model.ModelID{Provider: providerID, Name: declared.Name},
	)
	if err != nil {
		t.Fatalf("openTTS: %v", err)
	}
	if operations.Unary == nil || operations.Stream != nil {
		t.Fatalf("operations = %+v", operations)
	}
	request := speechRequest("你好。", &inference.AudioIntent{
		Voice: media.VoiceSpec{ID: "Cherry", Language: "Chinese"},
	})
	response, err := operations.Unary.Execute(
		context.Background(),
		model.ModelRef{ID: model.ModelID{Provider: providerID, Name: declared.Name}},
		request,
	)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(response.Message.Content.Parts) != 1 {
		t.Fatalf("parts = %#v", response.Message.Content.Parts)
	}
	audio, ok := response.Message.Content.Parts[0].(message.AudioPart)
	if !ok {
		t.Fatalf("part = %T", response.Message.Content.Parts[0])
	}
	if audio.Source.Kind() != media.SourceURL {
		t.Errorf("source kind = %v", audio.Source.Kind())
	}
	// The canonical alphabet has no WAV container token, so the URL source
	// declines to name a container it cannot name correctly.
	if audio.Source.MediaType() != "" {
		t.Errorf("source media type = %q", audio.Source.MediaType())
	}
	if !strings.HasSuffix(audio.Source.URL(), ".wav?Expires=1") {
		t.Errorf("source URL = %q", audio.Source.URL())
	}
	want := media.AudioFormat{
		Encoding:     media.AudioEncodingPCM16,
		SampleRateHz: 24000,
		Channels:     1,
	}
	if audio.Format == nil || *audio.Format != want {
		t.Errorf("format = %+v, want %+v", audio.Format, want)
	}
	if response.FinishReason != inference.FinishCompleted {
		t.Errorf("finish reason = %q", response.FinishReason)
	}
	if response.Usage.InputCharacters == nil || *response.Usage.InputCharacters != 12 {
		t.Errorf("characters = %v", response.Usage.InputCharacters)
	}
	if response.Metadata.RequestID != "req-tts-1" || response.Metadata.ResponseID != "audio_1" {
		t.Errorf("metadata = %+v", response.Metadata)
	}
	calls := platform.recorded()
	if len(calls) != 1 || calls[0].path != ttsPath {
		t.Fatalf("calls = %#v", calls)
	}
	if calls[0].async != "" || calls[0].sse != "" {
		t.Errorf("surface headers = async %q sse %q", calls[0].async, calls[0].sse)
	}
}

// TestTransportTTSClassifiesInBandFailure pins the refusal path: the platform
// reports speech failures in band under an HTTP 200, so the code field has to
// become the error rather than an empty success.
func TestTransportTTSClassifiesInBandFailure(t *testing.T) {
	platform := &fakePlatform{}
	platform.sync = func(map[string]any) (int, string) {
		return 200, `{"status_code":400,"request_id":"req-tts-2",` +
			`"code":"InvalidParameter","message":"voice not exist"}`
	}
	declared := testDeclaration(t, "qwen3-tts-flash")
	operations, err := openTTS(
		&clients{media: testClients(t, platform)}, declared,
		model.ModelID{Provider: providerID, Name: declared.Name},
	)
	if err != nil {
		t.Fatalf("openTTS: %v", err)
	}
	_, err = operations.Unary.Execute(
		context.Background(),
		model.ModelRef{ID: model.ModelID{Provider: providerID, Name: declared.Name}},
		speechRequest("你好。", &inference.AudioIntent{
			Voice: media.VoiceSpec{ID: "Nobody"},
		}),
	)
	if err == nil {
		t.Fatal("an in-band refusal surfaced as success")
	}
	if !unwrapContains(err, "voice not exist") {
		t.Errorf("error = %v", err)
	}
	if requestID, ok := errdefs.RequestID(err); !ok || requestID != "req-tts-2" {
		t.Errorf("request id = %q", requestID)
	}
}

// TestTransportTTSInlineAudio pins the other answer shape: a surface that
// returns the payload inline becomes bytes with the same media type rather
// than a URL that is not there.
func TestTransportTTSInlineAudio(t *testing.T) {
	platform := &fakePlatform{}
	platform.sync = func(map[string]any) (int, string) {
		return 200, `{"request_id":"req-tts-3","output":{"audio":` +
			`{"data":"UklGRg==","url":""}},` +
			`"usage":{"characters":3}}`
	}
	declared := testDeclaration(t, "qwen3-tts-flash")
	operations, err := openTTS(
		&clients{media: testClients(t, platform)}, declared,
		model.ModelID{Provider: providerID, Name: declared.Name},
	)
	if err != nil {
		t.Fatalf("openTTS: %v", err)
	}
	response, err := operations.Unary.Execute(
		context.Background(),
		model.ModelRef{ID: model.ModelID{Provider: providerID, Name: declared.Name}},
		speechRequest("你好。", &inference.AudioIntent{
			Voice: media.VoiceSpec{ID: "Cherry"},
		}),
	)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	audio := response.Message.Content.Parts[0].(message.AudioPart)
	if audio.Source.Kind() != media.SourceInline ||
		string(audio.Source.Bytes()) != "RIFF" {
		t.Errorf("source = %v %q", audio.Source.Kind(), audio.Source.Bytes())
	}
	if audio.Source.MediaType() != media.AudioEncodingPCM16.MediaType() {
		t.Errorf("source media type = %q", audio.Source.MediaType())
	}
}

// TestTransportTTSRefusesEmptyAudio pins the decode's fail-closed edge: an
// answer that names neither a URL nor a payload is a failure, not an empty
// success.
func TestTransportTTSRefusesEmptyAudio(t *testing.T) {
	platform := &fakePlatform{}
	platform.sync = func(map[string]any) (int, string) {
		return 200, `{"request_id":"req-tts-4","output":{"audio":{"data":"","url":""}}}`
	}
	declared := testDeclaration(t, "qwen3-tts-flash")
	operations, err := openTTS(
		&clients{media: testClients(t, platform)}, declared,
		model.ModelID{Provider: providerID, Name: declared.Name},
	)
	if err != nil {
		t.Fatalf("openTTS: %v", err)
	}
	_, err = operations.Unary.Execute(
		context.Background(),
		model.ModelRef{ID: model.ModelID{Provider: providerID, Name: declared.Name}},
		speechRequest("你好。", &inference.AudioIntent{
			Voice: media.VoiceSpec{ID: "Cherry"},
		}),
	)
	if err == nil || !unwrapContains(err, "returned no audio") {
		t.Errorf("error = %v", err)
	}
}

// unwrapContains walks an error's chain: the runtime wraps a provider failure
// without repeating its detail, so the cause is where a lane finds the
// platform's own words.
func unwrapContains(err error, detail string) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		if strings.Contains(current.Error(), detail) {
			return true
		}
	}
	return false
}
