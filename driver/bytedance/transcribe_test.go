package bytedance

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message/media"

	doubaospeech "github.com/GizClaw/doubao-speech-go"
)

func transcribeAudio(t *testing.T) media.AudioSource {
	t.Helper()
	source, err := media.NewAudioBytes(
		[]byte{0x00, 0x01, 0x02, 0x03},
		"audio/pcm",
	)
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	return source
}

func compileTranscribeWire(
	t *testing.T,
	request inference.TranscriptionRequest,
) (transcribeWire, inference.CompileReport, error) {
	t.Helper()
	compiled, err := compileTranscribe("doubao-seed-asr")(
		context.Background(),
		conformanceModel("doubao-seed-asr"),
		request,
	)
	return compiled.Wire, compiled.Report, err
}

func compileTranscribeSessionWire(
	t *testing.T,
	request inference.TranscriptionSessionRequest,
) (transcribeWire, inference.CompileReport, error) {
	t.Helper()
	compiled, err := compileTranscribeSession("doubao-seed-asr")(
		context.Background(),
		conformanceModel("doubao-seed-asr"),
		request,
	)
	return compiled.Wire, compiled.Report, err
}

func TestCompileTranscribe(t *testing.T) {
	wire, report, err := compileTranscribeWire(t, inference.TranscriptionRequest{
		Audio:      transcribeAudio(t),
		Language:   "zh-CN",
		Timestamps: true,
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if wire.resourceID != "doubao-seed-asr" {
		t.Fatalf("resourceID = %q", wire.resourceID)
	}
	if wire.format != doubaospeech.FormatPCM {
		t.Fatalf("format = %q, want pcm", wire.format)
	}
	if wire.language != doubaospeech.LanguageZhCN {
		t.Fatalf("language = %q, want zh-CN", wire.language)
	}
	if string(wire.audio) != string([]byte{0x00, 0x01, 0x02, 0x03}) {
		t.Fatalf("audio = %x", wire.audio)
	}
	for _, field := range []inference.FieldID{
		inference.FieldTranscriptionAudio,
		inference.FieldTranscriptionLanguage,
		inference.FieldTranscriptionTimestamps,
	} {
		if reason := rejectedReason(report, field); reason != "" {
			t.Fatalf("field %s rejected: %s", field, reason)
		}
	}
}

func TestCompileTranscribeRejects(t *testing.T) {
	t.Run("prompt", func(t *testing.T) {
		_, report, err := compileTranscribeWire(t, inference.TranscriptionRequest{
			Audio:  transcribeAudio(t),
			Prompt: "hint",
		})
		if reason := rejectedReason(
			report,
			inference.FieldTranscriptionPrompt,
		); !strings.Contains(reason, "no prompt control") {
			t.Fatalf("prompt reason = %q", reason)
		}
		if err == nil {
			t.Fatalf("compile succeeded, want prompt rejection")
		}
	})
	t.Run("unknown language", func(t *testing.T) {
		_, report, err := compileTranscribeWire(t, inference.TranscriptionRequest{
			Audio:    transcribeAudio(t),
			Language: "xx-YY",
		})
		if reason := rejectedReason(
			report,
			inference.FieldTranscriptionLanguage,
		); !strings.Contains(reason, "unsupported language") {
			t.Fatalf("language reason = %q", reason)
		}
		if err == nil {
			t.Fatalf("compile succeeded, want language rejection")
		}
	})
	t.Run("url audio", func(t *testing.T) {
		source, err := media.NewAudioURL(
			"https://example.com/input.wav",
			"audio/wav",
		)
		if err != nil {
			t.Fatalf("NewAudioURL: %v", err)
		}
		_, report, err := compileTranscribeWire(t, inference.TranscriptionRequest{
			Audio: source,
		})
		if reason := rejectedReason(
			report,
			inference.FieldTranscriptionAudio,
		); !strings.Contains(reason, "inline audio") {
			t.Fatalf("audio reason = %q", reason)
		}
		if err == nil {
			t.Fatalf("compile succeeded, want URL rejection")
		}
	})
	t.Run("extension", func(t *testing.T) {
		_, report, err := compileTranscribeWire(t, inference.TranscriptionRequest{
			Audio: transcribeAudio(t),
			Extensions: inference.Extensions{
				GenerateOptions{Provider: driverID, ServiceTier: "auto"},
			},
		})
		rejected := false
		for _, decision := range report.Decisions {
			if decision.Disposition == inference.Rejected &&
				strings.HasPrefix(string(decision.Field), "extension.") {
				rejected = true
			}
		}
		if !rejected {
			t.Fatalf("extension field not rejected: %+v", report.Decisions)
		}
		if err == nil {
			t.Fatalf("compile succeeded, want extension rejection")
		}
	})
}

func TestCompileTranscribeMediaTypes(t *testing.T) {
	tests := []struct {
		mediaType string
		format    doubaospeech.AudioFormat
		wantErr   bool
	}{
		{"audio/pcm", doubaospeech.FormatPCM, false},
		{"audio/mpeg", doubaospeech.FormatMP3, false},
		{"audio/opus", doubaospeech.FormatOGG, false},
		{"audio/aac", doubaospeech.FormatAAC, false},
		{"audio/wav", doubaospeech.FormatWAV, false},
		{"audio/x-wav", doubaospeech.FormatWAV, false},
		{"audio/flac", "", true},
		{"audio/ogg", "", true},
	}
	for _, test := range tests {
		t.Run(test.mediaType, func(t *testing.T) {
			source, err := media.NewAudioBytes([]byte{1}, test.mediaType)
			if err != nil {
				t.Fatalf("NewAudioBytes: %v", err)
			}
			wire, _, err := compileTranscribeWire(t, inference.TranscriptionRequest{
				Audio: source,
			})
			if test.wantErr {
				if err == nil {
					t.Fatalf("compile succeeded, want media type rejection")
				}
				return
			}
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if wire.format != test.format {
				t.Fatalf("format = %q, want %q", wire.format, test.format)
			}
		})
	}
}

func TestCompileTranscribeSession(t *testing.T) {
	request := func(format media.AudioFormat) inference.TranscriptionSessionRequest {
		return inference.TranscriptionSessionRequest{InputFormat: format}
	}
	tests := []struct {
		name        string
		format      media.AudioFormat
		wire        transcribeWire
		wantErr     bool
		errFragment string
	}{
		{
			name: "pcm16 mono",
			format: media.AudioFormat{
				Encoding:     media.AudioEncodingPCM16,
				SampleRateHz: 16000,
				Channels:     1,
			},
			wire: transcribeWire{
				format:     doubaospeech.FormatPCM,
				sampleRate: 16000,
				channel:    1,
				bits:       16,
			},
		},
		{
			name: "pcm16 stereo",
			format: media.AudioFormat{
				Encoding:     media.AudioEncodingPCM16,
				SampleRateHz: 48000,
				Channels:     2,
			},
			wire: transcribeWire{
				format:     doubaospeech.FormatPCM,
				sampleRate: 48000,
				channel:    2,
				bits:       16,
			},
		},
		{
			name:   "mp3",
			format: media.AudioFormat{Encoding: media.AudioEncodingMP3},
			wire:   transcribeWire{format: doubaospeech.FormatMP3},
		},
		{
			name:   "opus",
			format: media.AudioFormat{Encoding: media.AudioEncodingOpus},
			wire:   transcribeWire{format: doubaospeech.FormatOGG},
		},
		{
			name:   "aac",
			format: media.AudioFormat{Encoding: media.AudioEncodingAAC},
			wire:   transcribeWire{format: doubaospeech.FormatAAC},
		},
		{
			name:        "flac",
			format:      media.AudioFormat{Encoding: media.AudioEncodingFLAC},
			wantErr:     true,
			errFragment: "no native ASR token",
		},
		{
			name: "pcm24",
			format: media.AudioFormat{
				Encoding:     media.AudioEncodingPCM24,
				SampleRateHz: 16000,
				Channels:     1,
			},
			wantErr:     true,
			errFragment: "no native ASR token",
		},
		{
			name: "unsupported sample rate",
			format: media.AudioFormat{
				Encoding:     media.AudioEncodingPCM16,
				SampleRateHz: 11025,
				Channels:     1,
			},
			wantErr:     true,
			errFragment: "outside the provider's published set",
		},
		{
			name: "too many channels",
			format: media.AudioFormat{
				Encoding:     media.AudioEncodingPCM16,
				SampleRateHz: 16000,
				Channels:     3,
			},
			wantErr:     true,
			errFragment: "supports 1 or 2 channels",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wire, _, err := compileTranscribeSessionWire(t, request(test.format))
			if test.wantErr {
				if err == nil {
					t.Fatalf("compile succeeded, want %q", test.errFragment)
				}
				return
			}
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if wire.format != test.wire.format ||
				wire.sampleRate != test.wire.sampleRate ||
				wire.channel != test.wire.channel ||
				wire.bits != test.wire.bits {
				t.Fatalf(
					"wire = {format:%s rate:%d channel:%d bits:%d}, want {format:%s rate:%d channel:%d bits:%d}",
					wire.format, wire.sampleRate, wire.channel, wire.bits,
					test.wire.format, test.wire.sampleRate,
					test.wire.channel, test.wire.bits,
				)
			}
		})
	}
}

func TestCompileTranscribeSessionControls(t *testing.T) {
	wire, _, err := compileTranscribeSessionWire(t,
		inference.TranscriptionSessionRequest{
			InputFormat: media.AudioFormat{
				Encoding:     media.AudioEncodingPCM16,
				SampleRateHz: 16000,
				Channels:     1,
			},
			Language: "en-US",
		},
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if wire.language != doubaospeech.LanguageEnUS {
		t.Fatalf("language = %q, want en-US", wire.language)
	}

	_, _, err = compileTranscribeSessionWire(t,
		inference.TranscriptionSessionRequest{
			InputFormat: media.AudioFormat{
				Encoding:     media.AudioEncodingPCM16,
				SampleRateHz: 16000,
				Channels:     1,
			},
			Prompt: "hint",
		},
	)
	if err == nil {
		t.Fatalf("compile succeeded, want prompt rejection")
	}
}

func TestDecodeTranscribe(t *testing.T) {
	raw := transcribeRaw{
		text: "hello",
		utterances: []doubaospeech.ASRV2Utterance{{
			Text:      "hello",
			StartTime: 100,
			EndTime:   900,
			Words: []doubaospeech.ASRV2Word{
				{Text: "hello", StartTime: 100, EndTime: 900},
			},
		}},
		duration:  800,
		requestID: "req-1",
	}
	response, err := decodeTranscribe(context.Background(), raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Text != "hello" || len(response.Segments) != 1 {
		t.Fatalf("response = %+v", response)
	}
	segment := response.Segments[0]
	if segment.Text != "hello" ||
		segment.StartMillis != 100 ||
		segment.EndMillis != 900 ||
		len(segment.Words) != 1 {
		t.Fatalf("segment = %+v", segment)
	}
	if word := segment.Words[0]; word.Word != "hello" ||
		word.StartMillis != 100 || word.EndMillis != 900 {
		t.Fatalf("word = %+v", word)
	}
	if response.DurationMillis == nil || *response.DurationMillis != 800 {
		t.Fatalf("duration = %v", response.DurationMillis)
	}
	if response.Usage.AudioDurationMillis == nil ||
		*response.Usage.AudioDurationMillis != 800 {
		t.Fatalf("usage audio duration = %v", response.Usage.AudioDurationMillis)
	}
	if response.Metadata.RequestID != "req-1" {
		t.Fatalf("request id = %q", response.Metadata.RequestID)
	}
}

func TestDecodeTranscribeSessionEvent(t *testing.T) {
	partial, err := decodeTranscribeSessionEvent(
		context.Background(),
		&doubaospeech.ASRV2Result{Text: "par"},
	)
	if err != nil {
		t.Fatalf("decode partial: %v", err)
	}
	if partial.Text != "par" || partial.Final || partial.Segment != nil {
		t.Fatalf("partial event = %+v", partial)
	}

	final, err := decodeTranscribeSessionEvent(
		context.Background(),
		&doubaospeech.ASRV2Result{
			Text:    "full",
			IsFinal: true,
			Utterances: []doubaospeech.ASRV2Utterance{{
				Text:      "full",
				StartTime: 50,
				EndTime:   600,
				Words: []doubaospeech.ASRV2Word{
					{Text: "full", StartTime: 50, EndTime: 600},
				},
			}},
			ReqID:     "req-2",
			ConnectID: "conn-2",
		},
	)
	if err != nil {
		t.Fatalf("decode final: %v", err)
	}
	if !final.Final || final.Text != "" || final.Segment == nil ||
		final.RequestID != "req-2" || final.ResponseID != "conn-2" {
		t.Fatalf("final event = %+v", final)
	}
	if segment := final.Segment; segment.Text != "full" ||
		segment.StartMillis != 50 || segment.EndMillis != 600 ||
		len(segment.Words) != 1 {
		t.Fatalf("final segment = %+v", segment)
	}

	if _, err := decodeTranscribeSessionEvent(
		context.Background(),
		nil,
	); err == nil {
		t.Fatalf("nil raw decoded without error")
	}
}

func TestASRProviderSessionNext(t *testing.T) {
	results := []*doubaospeech.ASRV2Result{
		{Text: "partial", IsFinal: false},
		{Text: "final", IsFinal: true},
	}
	index := 0
	pull := func() (*doubaospeech.ASRV2Result, error, bool) {
		if index >= len(results) {
			return nil, nil, false
		}
		result := results[index]
		index++
		return result, nil, true
	}
	session := &asrProviderSession{pull: pull}

	first, err := session.Next(context.Background())
	if err != nil || first.Text != "partial" || first.IsFinal {
		t.Fatalf("first = %+v, %v", first, err)
	}
	second, err := session.Next(context.Background())
	if err != nil || !second.IsFinal || second.Text != "final" {
		t.Fatalf("second = %+v, %v", second, err)
	}
	if _, err := session.Next(context.Background()); err != io.EOF {
		t.Fatalf("third = %v, want io.EOF", err)
	}
}

func TestASRProviderSessionContinuous(t *testing.T) {
	results := []*doubaospeech.ASRV2Result{
		{Text: "one", IsFinal: false},
		{Text: "one", IsFinal: true},
		{Text: "two", IsFinal: false},
		{Text: "two", IsFinal: true},
	}
	index := 0
	pull := func() (*doubaospeech.ASRV2Result, error, bool) {
		if index >= len(results) {
			return nil, nil, false
		}
		result := results[index]
		index++
		return result, nil, true
	}
	session := &asrProviderSession{pull: pull}
	for _, want := range results {
		got, err := session.Next(context.Background())
		if err != nil || got != want {
			t.Fatalf("Next = %+v, %v; want %+v", got, err, want)
		}
	}
	if _, err := session.Next(context.Background()); err != io.EOF {
		t.Fatalf("Next after results = %v, want io.EOF", err)
	}
}

func TestASRProviderSessionFinishInput(t *testing.T) {
	results := []*doubaospeech.ASRV2Result{
		{Text: "partial", IsFinal: false},
		{Text: "one", IsFinal: true},
		{Text: "two", IsFinal: true},
	}
	index := 0
	pull := func() (*doubaospeech.ASRV2Result, error, bool) {
		if index >= len(results) {
			return nil, nil, false
		}
		result := results[index]
		index++
		return result, nil, true
	}
	var lastFlags []bool
	sendAudio := func(_ context.Context, _ []byte, isLast bool) error {
		lastFlags = append(lastFlags, isLast)
		return nil
	}
	session := &asrProviderSession{
		pull:      pull,
		sendAudio: sendAudio,
	}

	first, err := session.Next(context.Background())
	if err != nil || first.IsFinal {
		t.Fatalf("first = %+v, %v", first, err)
	}
	second, err := session.Next(context.Background())
	if err != nil || !second.IsFinal || second.Text != "one" {
		t.Fatalf("second = %+v, %v; want continuous final", second, err)
	}

	if err := session.FinishInput(context.Background()); err != nil {
		t.Fatalf("FinishInput: %v", err)
	}
	if len(lastFlags) != 1 || !lastFlags[0] {
		t.Fatalf("finish frame = %v, want one isLast=true send", lastFlags)
	}
	// Idempotent: a second finish must not re-send the frame.
	if err := session.FinishInput(context.Background()); err != nil {
		t.Fatalf("second FinishInput: %v", err)
	}
	if len(lastFlags) != 1 {
		t.Fatalf("finish frames = %d, want 1", len(lastFlags))
	}
	// No more audio may arrive after finish.
	if err := session.Send(
		context.Background(),
		media.AudioChunk{Data: []byte{1}},
	); err == nil {
		t.Fatalf("Send after FinishInput succeeded")
	}

	third, err := session.Next(context.Background())
	if err != nil || !third.IsFinal || third.Text != "two" {
		t.Fatalf("post-finish final = %+v, %v", third, err)
	}
	if _, err := session.Next(context.Background()); err != io.EOF {
		t.Fatalf("Next after post-finish final = %v, want io.EOF", err)
	}
}

func TestASRProviderSessionFinishEndsWithoutFinal(t *testing.T) {
	exhausted := false
	pull := func() (*doubaospeech.ASRV2Result, error, bool) {
		if exhausted {
			return nil, nil, false
		}
		exhausted = true
		return &doubaospeech.ASRV2Result{Text: "partial"}, nil, true
	}
	session := &asrProviderSession{
		pull:      pull,
		sendAudio: func(context.Context, []byte, bool) error { return nil },
	}
	first, err := session.Next(context.Background())
	if err != nil || first.Text != "partial" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	if err := session.FinishInput(context.Background()); err != nil {
		t.Fatalf("FinishInput: %v", err)
	}
	if _, err := session.Next(context.Background()); err != io.EOF {
		t.Fatalf("Next after finish = %v, want io.EOF", err)
	}
}

func TestASRProviderSessionPullError(t *testing.T) {
	pullErr := errors.New("wire failure")
	session := &asrProviderSession{
		pull: func() (*doubaospeech.ASRV2Result, error, bool) {
			return nil, pullErr, true
		},
	}
	first, err := session.Next(context.Background())
	if first != nil || err == nil {
		t.Fatalf("Next = %+v, %v", first, err)
	}
	if _, err := session.Next(context.Background()); err == nil {
		t.Fatalf("second Next succeeded after failure")
	}
}

func TestASRProviderSessionCloseAndInterrupt(t *testing.T) {
	session := &asrProviderSession{
		pull: func() (*doubaospeech.ASRV2Result, error, bool) {
			return nil, nil, false
		},
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := session.Interrupt(); err != nil {
		t.Fatalf("Interrupt after Close: %v", err)
	}
	if _, err := session.Next(context.Background()); err != io.EOF {
		t.Fatalf("Next after Close = %v, want io.EOF", err)
	}
}

func conformanceTranscribeRaw() transcribeRaw {
	return transcribeRaw{
		text: "ok",
		utterances: []doubaospeech.ASRV2Utterance{{
			Text: "ok",
		}},
		requestID: "req-1",
	}
}

func TestConformanceTranscribeUnary(t *testing.T) {
	calls := &inferencetest.Counter{}
	driver, err := inference.BindTranscribe(
		compileTranscribe("doubao-seed-asr"),
		countingTransport(calls, func(
			_ context.Context,
			_ transcribeWire,
		) (transcribeRaw, error) {
			return conformanceTranscribeRaw(), nil
		}),
		decodeTranscribe,
	)
	if err != nil {
		t.Fatalf("BindTranscribe: %v", err)
	}
	inferencetest.RunTranscribeUnary(t, inferencetest.TranscriptionUnarySuite{
		Model: conformanceModel("doubao-seed-asr"),
		Request: func() inference.TranscriptionRequest {
			return inference.TranscriptionRequest{Audio: transcribeAudio(t)}
		},
		Driver:         driver,
		TransportCalls: calls.Load,
	})
}

// conformanceASRSession plays back provider results for the session
// conformance suite without touching the network.
type conformanceASRSession struct {
	results []*doubaospeech.ASRV2Result
	index   int
	sends   int
}

func (s *conformanceASRSession) Send(
	context.Context,
	media.AudioChunk,
) error {
	s.sends++
	return nil
}

func (s *conformanceASRSession) Next(
	context.Context,
) (*doubaospeech.ASRV2Result, error) {
	if s.index >= len(s.results) {
		return nil, io.EOF
	}
	result := s.results[s.index]
	s.index++
	return result, nil
}

func (*conformanceASRSession) Interrupt() error { return nil }
func (*conformanceASRSession) Close() error     { return nil }

func countingTranscribeSessionTransport(
	calls *inferencetest.Counter,
	next inference.TranscriptionSessionTransport[
		transcribeWire,
		*doubaospeech.ASRV2Result,
	],
) inference.TranscriptionSessionTransport[
	transcribeWire,
	*doubaospeech.ASRV2Result,
] {
	return func(
		ctx context.Context,
		wire transcribeWire,
	) (inference.ProviderSession[*doubaospeech.ASRV2Result], error) {
		calls.Inc()
		return next(ctx, wire)
	}
}

func TestConformanceTranscribeSession(t *testing.T) {
	calls := &inferencetest.Counter{}
	driver, err := inference.BindTranscribeSession(
		compileTranscribeSession("doubao-seed-asr"),
		countingTranscribeSessionTransport(
			calls,
			func(
				_ context.Context,
				_ transcribeWire,
			) (inference.ProviderSession[*doubaospeech.ASRV2Result], error) {
				return &conformanceASRSession{results: []*doubaospeech.ASRV2Result{
					{Text: "partial", IsFinal: false},
					{
						Text:    "ok",
						IsFinal: true,
						Utterances: []doubaospeech.ASRV2Utterance{{
							Text:      "ok",
							StartTime: 0,
							EndTime:   300,
						}},
						ReqID: "req-1",
					},
				}}, nil
			},
		),
		decodeTranscribeSessionEvent,
	)
	if err != nil {
		t.Fatalf("BindTranscribeSession: %v", err)
	}
	inferencetest.RunTranscribeSession(t, inferencetest.TranscriptionSessionSuite{
		Model: conformanceModel("doubao-seed-asr"),
		Request: func() inference.TranscriptionSessionRequest {
			return inference.TranscriptionSessionRequest{
				InputFormat: media.AudioFormat{
					Encoding:     media.AudioEncodingPCM16,
					SampleRateHz: 16000,
					Channels:     1,
				},
			}
		},
		Driver:         driver,
		TransportCalls: calls.Load,
		AssertEvent: func(t *testing.T, event inference.TranscriptionSessionEvent) {
			t.Helper()
			if event.Text == "partial" {
				return
			}
			if !event.Final || event.Segment == nil ||
				event.Segment.Text != "ok" {
				t.Fatalf("event = %+v", event)
			}
		},
		AssertResult: func(t *testing.T, response inference.TranscriptionResponse) {
			t.Helper()
			if response.Text != "ok" || len(response.Segments) != 1 ||
				response.Segments[0].Text != "ok" {
				t.Fatalf("result = %+v", response)
			}
			if response.Metadata.RequestID != "req-1" {
				t.Fatalf("request id = %q", response.Metadata.RequestID)
			}
		},
	})
}
