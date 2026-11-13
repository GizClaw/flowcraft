package minimax

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// speechSettings builds a deployment that serves one speech model against base.
func speechSettings(base string) Settings {
	return Settings{
		BaseURL: base,
		APIKey:  "test-key",
		Models:  []ModelSpec{speechSpec()},
	}
}

func TestSpeechUnaryDecodesHexAudio(t *testing.T) {
	// 4800 bytes of 24kHz mono pcm16 is 100ms, so the decoder can derive a
	// duration rather than reporting an unknown one.
	audio := make([]byte, 4800)
	for i := range audio {
		audio[i] = byte(i)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ttsPath {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body["output_format"] != "hex" || body["stream"] != false {
			t.Errorf("request = %+v", body)
		}
		if body["model"] != "speech-2.6-hd" {
			t.Errorf("model = %v", body["model"])
		}
		// The language defaults to auto, so a caller that names none still sends
		// the field the endpoint expects.
		if body["language_boost"] != "auto" {
			t.Errorf("language_boost = %v, want auto", body["language_boost"])
		}
		voice, _ := body["voice_setting"].(map[string]any)
		if voice["voice_id"] != "v" {
			t.Errorf("voice = %+v", voice)
		}
		// The negotiated format is the one sent, with the canonical encoding
		// names mapped onto the wire tokens by hand here (not via the driver's
		// own inverse mapping).
		audioSettings, _ := body["audio_setting"].(map[string]any)
		if audioSettings["format"] != "pcm" {
			t.Errorf("audio format = %v, want pcm", audioSettings["format"])
		}
		if audioSettings["sample_rate"] != float64(24000) {
			t.Errorf("sample_rate = %v, want 24000", audioSettings["sample_rate"])
		}
		if audioSettings["channel"] != float64(1) {
			t.Errorf("channel = %v, want 1", audioSettings["channel"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":      map[string]any{"audio": hex.EncodeToString(audio)},
			"trace_id":  "trace-1",
			"base_resp": map[string]any{"status_code": 0, "status_msg": "success"},
		})
	}))
	defer server.Close()

	provider, err := New(speechSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	response, err := provider.Generate(context.Background(), speechRequest("hello",
		&TTSOptions{Voice: "v", Format: media.AudioEncodingPCM16,
			SampleRateHz: 24000, Channels: 1}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.FinishReason != inference.FinishStop {
		t.Fatalf("finish = %q", response.FinishReason)
	}
	if response.Metadata.RequestID != "trace-1" {
		t.Fatalf("request id = %q", response.Metadata.RequestID)
	}
	part, ok := response.Message.Content.Parts[0].(message.AudioPart)
	if !ok {
		t.Fatalf("part = %T", response.Message.Content.Parts[0])
	}
	if part.Format.Encoding != media.AudioEncodingPCM16 ||
		part.Format.SampleRateHz != 24000 || part.Format.Channels != 1 {
		t.Fatalf("format = %+v", part.Format)
	}
	if got := part.Source.Bytes(); string(got) != string(audio) {
		t.Fatalf("audio = %v", got)
	}
	if part.DurationMillis != 100 {
		t.Fatalf("pcm duration = %d, want 100ms", part.DurationMillis)
	}
	if got := response.Usage.Work[usage.WorkInputCharacters]; got != int64(len("hello")) {
		t.Fatalf("input characters = %d", got)
	}
}

func TestSpeechUnaryClassifiesEnvelopeFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":      map[string]any{"audio": ""},
			"trace_id":  "trace-2",
			"base_resp": map[string]any{"status_code": statusRateLimited, "status_msg": "slow down"},
		})
	}))
	defer server.Close()

	provider, err := New(speechSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.Generate(context.Background(), speechRequest("hello", &TTSOptions{Voice: "v"}))
	if err == nil {
		t.Fatal("rate-limited call returned no error")
	}
	if !errdefs.IsRateLimit(err) {
		t.Fatalf("error = %v, want a rate limit classification", err)
	}
}

func TestSpeechStreamEmitsChunks(t *testing.T) {
	first := []byte{0x10, 0x11}
	second := []byte{0x12, 0x13}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body["stream"] != true {
			t.Errorf("stream = %v, want true", body["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		events := []map[string]any{
			{"data": map[string]any{"audio": hex.EncodeToString(first), "status": 1}, "trace_id": "trace-3"},
			{"data": map[string]any{"audio": hex.EncodeToString(second), "status": 1}, "trace_id": "trace-3"},
			{"data": map[string]any{"audio": "", "status": 2}, "trace_id": "trace-3"},
		}
		for _, event := range events {
			raw, _ := json.Marshal(event)
			_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
		}
	}))
	defer server.Close()

	provider, err := New(speechSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := provider.GenerateStream(context.Background(),
		speechRequest("hello", &TTSOptions{Voice: "v"}))
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var (
		kinds  []inference.StreamEventKind
		chunks [][]byte
	)
	for {
		event, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		kinds = append(kinds, event.Kind)
		if event.Kind == inference.StreamChunk {
			part, ok := event.Part.(message.AudioPart)
			if !ok {
				t.Fatalf("chunk part = %T", event.Part)
			}
			if part.Format.Encoding != media.AudioEncodingMP3 {
				t.Fatalf("chunk format = %+v", part.Format)
			}
			chunks = append(chunks, part.Source.Bytes())
		}
	}
	if len(kinds) < 3 || kinds[0] != inference.StreamStart ||
		kinds[len(kinds)-1] != inference.StreamEnd {
		t.Fatalf("events = %v", kinds)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	if string(chunks[0]) != string(first) || string(chunks[1]) != string(second) {
		t.Fatalf("chunk bytes = %v", chunks)
	}

	result, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	part, ok := result.Message.Content.Parts[0].(message.AudioPart)
	if !ok {
		t.Fatalf("assembled part = %T", result.Message.Content.Parts[0])
	}
	joined := append(append([]byte{}, first...), second...)
	if string(part.Source.Bytes()) != string(joined) {
		t.Fatalf("assembled audio = %v", part.Source.Bytes())
	}
}

func TestSpeechStreamFailsOnTruncation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		raw, _ := json.Marshal(map[string]any{
			"data":     map[string]any{"audio": hex.EncodeToString([]byte{1}), "status": 1},
			"trace_id": "trace-4",
		})
		_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
	}))
	defer server.Close()

	provider, err := New(speechSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := provider.GenerateStream(context.Background(),
		speechRequest("hello", &TTSOptions{Voice: "v"}))
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var terminal error
	for {
		_, err := stream.Next(context.Background())
		if err != nil {
			terminal = err
			break
		}
	}
	if !errors.Is(terminal, io.EOF) {
		if !isErrorKind(terminal, inference.ProviderTruncated) {
			t.Fatalf("error = %v, want truncation", terminal)
		}
	}
}

func TestSpeechStreamRejectsVideoModel(t *testing.T) {
	provider, err := New(Settings{
		BaseURL: "https://example.invalid",
		APIKey:  "test-key",
		Models:  []ModelSpec{h3Spec()},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.GenerateStream(context.Background(),
		testRequest("MiniMax-H3", inference.OutputVideo, message.NewTextContent("a")))
	if !isErrorKind(err, inference.UnsupportedOperation) {
		t.Fatalf("error = %v, want an unsupported operation", err)
	}
}

func TestUndeclaredModelFailsClosed(t *testing.T) {
	provider, err := New(Settings{BaseURL: "https://example.invalid", APIKey: "test-key"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.Generate(context.Background(),
		testRequest("speech-2.6-hd", inference.OutputAudio, message.NewTextContent("hi")))
	if !isErrorKind(err, inference.UnknownModel) {
		t.Fatalf("error = %v, want unknown_model", err)
	}
}

func TestSSEEventsSkipsNonDataLines(t *testing.T) {
	body := strings.NewReader(": keep-alive\n\nevent: message\ndata: {\"a\":1}\n\n")
	var payloads []string
	if err := sseEvents(body, func(payload []byte) bool {
		payloads = append(payloads, string(payload))
		return true
	}); err != nil {
		t.Fatalf("sseEvents: %v", err)
	}
	if len(payloads) != 1 || payloads[0] != `{"a":1}` {
		t.Fatalf("payloads = %v", payloads)
	}
}

// TestSpeechWireFormatTokens pins every served encoding's wire token against the
// literal the platform documents. Without this, ttsFormatToken and ttsEncodingOf
// could agree on a wrong vocabulary and every other test would still pass.
func TestSpeechWireFormatTokens(t *testing.T) {
	cases := []struct {
		encoding media.AudioEncoding
		token    string
	}{
		{media.AudioEncodingMP3, "mp3"},
		{media.AudioEncodingPCM16, "pcm"},
		{media.AudioEncodingFLAC, "flac"},
		{media.AudioEncodingOpus, "opus"},
	}
	for _, tc := range cases {
		t.Run(string(tc.encoding), func(t *testing.T) {
			options := &TTSOptions{Voice: "v", Format: tc.encoding}
			if tc.encoding == media.AudioEncodingPCM16 {
				options.SampleRateHz = 24000
				options.Channels = 1
			}
			wire, err := compileSpeech(t, speechRequest("hello", options), protocol.ShapeUnary)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			body := ttsRequest(wire)
			audioSettings, _ := body["audio_setting"].(map[string]any)
			if audioSettings["format"] != tc.token {
				t.Fatalf("format = %v, want %q", audioSettings["format"], tc.token)
			}
		})
	}
}

// TestSpeechDeclarationSuppliesTheFormat covers the path where the call names no
// format and the deployment's declaration does.
func TestSpeechDeclarationSuppliesTheFormat(t *testing.T) {
	declared := speechSpec()
	declared.TTS = TTSParams{
		Encoding: media.AudioEncodingPCM16, SampleRateHz: 16000, Channels: 2,
	}
	compiled, err := compileTTS(declared, "speech-2.6-hd")(context.Background(),
		speechRequest("hello", &TTSOptions{Voice: "v"}), protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	body := ttsRequest(compiled.Wire)
	audioSettings, _ := body["audio_setting"].(map[string]any)
	if audioSettings["format"] != "pcm" ||
		audioSettings["sample_rate"] != 16000 ||
		audioSettings["channel"] != 2 {
		t.Fatalf("audio setting = %+v", audioSettings)
	}
}

// TestSpeechSendsSpeed pins the speed multiplier on the voice setting.
func TestSpeechSendsSpeed(t *testing.T) {
	wire, err := compileSpeech(t, speechRequest("hello", &TTSOptions{
		Voice: "v", Speed: floatPtr(1.25),
	}), protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	voice, _ := ttsRequest(wire)["voice_setting"].(map[string]any)
	if voice["speed"] != 1.25 {
		t.Fatalf("speed = %v, want 1.25", voice["speed"])
	}
}

// TestStreamChannelKeepsNoReadAhead pins the pump's buffer: the consumer is a pull
// loop, and each buffered event can hold a whole SSE line.
func TestStreamChannelKeepsNoReadAhead(t *testing.T) {
	stream := newTTSStream(io.NopCloser(strings.NewReader("")), media.AudioFormat{}, 0)
	defer func() { _ = stream.Close() }()
	if capacity := cap(stream.events); capacity > 1 {
		t.Fatalf("event buffer = %d, want no read-ahead", capacity)
	}
}

// sseServer serves the given raw event lines on the speech path, so each negative
// case states only its own stream.
func sseServer(t *testing.T, events ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			_, _ = w.Write([]byte("data: " + event + "\n\n"))
		}
	}))
}

// drain pulls a stream to its terminal state and returns the events seen so far.
func drain(t *testing.T, stream inference.Stream) ([]inference.StreamEvent, error) {
	t.Helper()
	var events []inference.StreamEvent
	for {
		event, err := stream.Next(context.Background())
		if err != nil {
			if errors.Is(err, io.EOF) {
				return events, nil
			}
			return events, err
		}
		events = append(events, event)
	}
}

// TestSpeechStreamKeepsTheFinalChunk covers the branch where the terminal event
// still carries audio: those bytes belong to the utterance, not to the end marker.
func TestSpeechStreamKeepsTheFinalChunk(t *testing.T) {
	first := hex.EncodeToString([]byte{1, 2})
	last := hex.EncodeToString([]byte{3, 4})
	server := sseServer(t,
		`{"data":{"audio":"`+first+`","status":1},"trace_id":"tr-final"}`,
		`{"data":{"audio":"`+last+`","status":2},"trace_id":"tr-final"}`,
	)
	defer server.Close()

	provider, err := New(speechSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := provider.GenerateStream(context.Background(),
		speechRequest("hello", &TTSOptions{Voice: "v"}))
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	events, err := drain(t, stream)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	chunks := 0
	for _, event := range events {
		if event.Kind == inference.StreamChunk {
			chunks++
		}
	}
	if chunks != 2 {
		t.Fatalf("chunks = %d, want both the payload chunk and the terminal one", chunks)
	}
	if events[len(events)-1].Kind != inference.StreamEnd {
		t.Fatalf("last event = %s", events[len(events)-1].Kind)
	}
	result, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	part, ok := result.Message.Content.Parts[0].(message.AudioPart)
	if !ok {
		t.Fatalf("part = %T", result.Message.Content.Parts[0])
	}
	if len(part.Source.Bytes()) != 4 {
		t.Fatalf("assembled audio = %v, want four bytes", part.Source.Bytes())
	}
}

// TestSpeechStreamIgnoresProgressEvents covers a status-1 event with no audio: it
// reports progress, not content.
func TestSpeechStreamIgnoresProgressEvents(t *testing.T) {
	server := sseServer(t,
		`{"data":{"audio":"`+hex.EncodeToString([]byte{1})+`","status":1},"trace_id":"tr-p"}`,
		`{"data":{"audio":"","status":1},"trace_id":"tr-p"}`,
		`{"data":{"audio":"`+hex.EncodeToString([]byte{2})+`","status":1},"trace_id":"tr-p"}`,
		`{"data":{"audio":"","status":2},"trace_id":"tr-p"}`,
	)
	defer server.Close()

	provider, err := New(speechSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := provider.GenerateStream(context.Background(),
		speechRequest("hello", &TTSOptions{Voice: "v"}))
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	events, err := drain(t, stream)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	chunks := 0
	for _, event := range events {
		if event.Kind == inference.StreamChunk {
			chunks++
		}
	}
	if chunks != 2 {
		t.Fatalf("chunks = %d, want the two audio events only", chunks)
	}
}

// TestSpeechStreamClassifiesMidStreamFailure covers a failure envelope arriving
// after the stream opened: the class must survive, not become a truncation.
func TestSpeechStreamClassifiesMidStreamFailure(t *testing.T) {
	server := sseServer(t,
		`{"data":{"audio":"`+hex.EncodeToString([]byte{1})+`","status":1},"trace_id":"tr-f"}`,
		`{"base_resp":{"status_code":`+strconv.Itoa(statusRateLimited)+`,"status_msg":"slow down"}}`,
	)
	defer server.Close()

	provider, err := New(speechSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := provider.GenerateStream(context.Background(),
		speechRequest("hello", &TTSOptions{Voice: "v"}))
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	_, err = drain(t, stream)
	if !errdefs.IsRateLimit(err) {
		t.Fatalf("error = %v, want the platform's rate-limit class", err)
	}
}

// TestSpeechStreamRejectsMalformedEvent covers a payload that is not JSON.
func TestSpeechStreamRejectsMalformedEvent(t *testing.T) {
	server := sseServer(t, `{"data":`)
	defer server.Close()

	provider, err := New(speechSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := provider.GenerateStream(context.Background(),
		speechRequest("hello", &TTSOptions{Voice: "v"}))
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if _, err := drain(t, stream); !isErrorKind(err, inference.InvalidProviderResponse) {
		t.Fatalf("error = %v, want an invalid provider response", err)
	}
}

// TestSpeechStreamRejectsNonHexAudio covers a chunk that is not hex.
func TestSpeechStreamRejectsNonHexAudio(t *testing.T) {
	server := sseServer(t,
		`{"data":{"audio":"zz","status":1},"trace_id":"tr-h"}`,
	)
	defer server.Close()

	provider, err := New(speechSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := provider.GenerateStream(context.Background(),
		speechRequest("hello", &TTSOptions{Voice: "v"}))
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if _, err := drain(t, stream); !isErrorKind(err, inference.InvalidProviderResponse) {
		t.Fatalf("error = %v, want an invalid provider response", err)
	}
}

// TestSpeechStreamCloseIsIdempotent covers the pull contract's teardown.
func TestSpeechStreamCloseIsIdempotent(t *testing.T) {
	server := sseServer(t, `{"data":{"audio":"`+hex.EncodeToString([]byte{1})+`","status":2}}`)
	defer server.Close()

	provider, err := New(speechSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := provider.GenerateStream(context.Background(),
		speechRequest("hello", &TTSOptions{Voice: "v"}))
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestSpeechPromptLimitBoundary pins both sides of the declared character cap.
func TestSpeechPromptLimitBoundary(t *testing.T) {
	declared := speechSpec()
	declared.TTS = TTSParams{PromptLimit: 4}
	if _, err := compileTTS(declared, "speech-2.6-hd")(context.Background(),
		speechRequest("1234", &TTSOptions{Voice: "v"}), protocol.ShapeUnary); err != nil {
		t.Fatalf("text at the limit rejected: %v", err)
	}
	if _, err := compileTTS(declared, "speech-2.6-hd")(context.Background(),
		speechRequest("12345", &TTSOptions{Voice: "v"}), protocol.ShapeUnary); !isErrorKind(err, inference.InvalidRequest) {
		t.Fatalf("text above the limit: error = %v", err)
	}
}

// TestSpeechUnaryUsesPlatformAccounting pins the platform's own numbers over the
// driver's estimates: the live endpoint bills more "characters" than the text has
// runes and reports the exact clip length, so metering must follow the platform.
func TestSpeechUnaryUsesPlatformAccounting(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":     map[string]any{"audio": hex.EncodeToString(make([]byte, 512))},
			"trace_id": "trace-accounting",
			"extra_info": map[string]any{
				"audio_length": 2736, "audio_sample_rate": 32000,
				"audio_channel": 1, "audio_format": "mp3", "usage_characters": 22,
			},
			"base_resp": map[string]any{"status_code": 0},
		})
	}))
	defer server.Close()

	provider, err := New(speechSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// "hello" is five runes; the platform bills its own count.
	response, err := provider.Generate(context.Background(),
		speechRequest("hello", &TTSOptions{Voice: "v", Format: media.AudioEncodingMP3}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got := response.Usage.Work[usage.WorkInputCharacters]; got != 22 {
		t.Fatalf("characters = %d, want the platform's 22", got)
	}
	if got := response.Usage.Work[usage.WorkAudioDurationMillis]; got != 2736 {
		t.Fatalf("duration = %d, want the platform's 2736", got)
	}
	part, ok := response.Message.Content.Parts[0].(message.AudioPart)
	if !ok {
		t.Fatalf("part = %T", response.Message.Content.Parts[0])
	}
	if part.DurationMillis != 2736 {
		t.Fatalf("part duration = %d", part.DurationMillis)
	}
}

// TestSpeechStreamReportsTraceAndUsage covers the parity gap the streamed path had:
// the terminal event carries the trace and the accounting, so the assembled Result
// must too.
func TestSpeechStreamReportsTraceAndUsage(t *testing.T) {
	server := sseServer(t,
		`{"data":{"audio":"`+hex.EncodeToString([]byte{1, 2})+`","status":1},"trace_id":"tr-usage"}`,
		`{"data":{"audio":"","status":2},"trace_id":"tr-usage","extra_info":{"audio_length":1439,"usage_characters":7,"audio_format":"mp3"}}`,
	)
	defer server.Close()

	provider, err := New(speechSettings(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := provider.GenerateStream(context.Background(),
		speechRequest("嗨，测试", &TTSOptions{Voice: "v"}))
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if _, err := drain(t, stream); err != nil {
		t.Fatalf("drain: %v", err)
	}
	result, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if result.Metadata.RequestID != "tr-usage" {
		t.Fatalf("request id = %q, want the streamed trace", result.Metadata.RequestID)
	}
	if got := result.Usage.Work[usage.WorkInputCharacters]; got != 7 {
		t.Fatalf("characters = %d, want the platform's 7", got)
	}
	if got := result.Usage.Work[usage.WorkAudioDurationMillis]; got != 1439 {
		t.Fatalf("duration = %d, want the platform's 1439", got)
	}
}
