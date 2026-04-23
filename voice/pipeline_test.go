package voice_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/workflow"
	"github.com/GizClaw/flowcraft/voice"
	"github.com/GizClaw/flowcraft/voice/audio"
	"github.com/GizClaw/flowcraft/voice/provider"
	"github.com/GizClaw/flowcraft/voice/stt"
	"github.com/GizClaw/flowcraft/voice/tts"
)

func collectStreamEvents(t *testing.T, s audio.Stream[voice.Event]) []voice.Event {
	t.Helper()
	var events []voice.Event
	for {
		ev, err := s.Read()
		if err != nil {
			break
		}
		events = append(events, ev)
	}
	return events
}

// ---------------------------------------------------------------------------
// Fake implementations
// ---------------------------------------------------------------------------

type fakeSTT struct {
	preset string
}

func (f *fakeSTT) Recognize(ctx context.Context, input audio.Frame, opts ...stt.STTOption) (stt.STTResult, error) {
	return stt.STTResult{Text: f.preset, IsFinal: true, Audio: input}, nil
}

type fakeStreamSTT struct {
	preset string
}

func (f *fakeStreamSTT) Recognize(ctx context.Context, input audio.Frame, opts ...stt.STTOption) (stt.STTResult, error) {
	return stt.STTResult{Text: f.preset, IsFinal: true, Audio: input}, nil
}

func (f *fakeStreamSTT) RecognizeStream(ctx context.Context, input audio.Stream[audio.Frame], opts ...stt.STTOption) (audio.Stream[stt.STTResult], error) {
	out := audio.NewPipe[stt.STTResult](8)
	go func() {
		defer out.Close()
		for {
			_, err := input.Read()
			if err != nil {
				if err == io.EOF {
					out.Send(stt.STTResult{Text: f.preset, IsFinal: true, Audio: audio.Frame{}})
				}
				return
			}
		}
	}()
	return out, nil
}

type tailingStreamSTT struct{}

func (tailingStreamSTT) Recognize(context.Context, audio.Frame, ...stt.STTOption) (stt.STTResult, error) {
	return stt.STTResult{Text: "hello", IsFinal: true}, nil
}

func (tailingStreamSTT) RecognizeStream(context.Context, audio.Stream[audio.Frame], ...stt.STTOption) (audio.Stream[stt.STTResult], error) {
	out := audio.NewPipe[stt.STTResult](4)
	go func() {
		defer out.Close()
		out.Send(stt.STTResult{Text: "hello", IsFinal: true})
		out.Send(stt.STTResult{Text: "hello wor", IsFinal: false})
		out.Send(stt.STTResult{Text: "hello world", IsFinal: true})
	}()
	return out, nil
}

type metadataSTT struct{}

func (metadataSTT) Recognize(context.Context, audio.Frame, ...stt.STTOption) (stt.STTResult, error) {
	return stt.STTResult{
		Text:       "hello",
		IsFinal:    true,
		Lang:       "en-US",
		Confidence: 0.92,
		Duration:   1500 * time.Millisecond,
		Words: []stt.WordTiming{
			{Word: "hello", Start: 0, End: 500 * time.Millisecond},
		},
	}, nil
}

type metadataStreamSTT struct{}

func (metadataStreamSTT) Recognize(context.Context, audio.Frame, ...stt.STTOption) (stt.STTResult, error) {
	return stt.STTResult{}, nil
}

func (metadataStreamSTT) RecognizeStream(context.Context, audio.Stream[audio.Frame], ...stt.STTOption) (audio.Stream[stt.STTResult], error) {
	out := audio.NewPipe[stt.STTResult](2)
	go func() {
		defer out.Close()
		out.Send(stt.STTResult{
			Text:       "hel",
			IsFinal:    false,
			Lang:       "en-US",
			Confidence: 0.5,
		})
		out.Send(stt.STTResult{
			Text:       "hello",
			IsFinal:    true,
			Lang:       "en-US",
			Confidence: 0.95,
			Duration:   1200 * time.Millisecond,
			Words: []stt.WordTiming{
				{Word: "hello", Start: 0, End: 400 * time.Millisecond},
			},
		})
	}()
	return out, nil
}

type fakeTTS struct{}

func (f *fakeTTS) Synthesize(ctx context.Context, text string, opts ...tts.TTSOption) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader([]byte("audio:" + text))), nil
}

func (f *fakeTTS) Voices(ctx context.Context) ([]tts.Voice, error) {
	return nil, nil
}

type fakeStreamTTS struct {
	*fakeTTS
}

func (f *fakeStreamTTS) SynthesizeStream(ctx context.Context, input audio.Stream[string], opts ...tts.TTSOption) (audio.Stream[tts.Utterance], error) {
	out := audio.NewPipe[tts.Utterance](8)
	go func() {
		defer out.Close()
		seq := 0
		for {
			sentence, err := input.Read()
			if err != nil {
				return
			}
			data := []byte("audio:" + sentence)
			if !out.Send(tts.Utterance{Frame: audio.Frame{Data: data}, Text: sentence, Sequence: seq}) {
				return
			}
			seq++
		}
	}()
	return out, nil
}

type failingSTTAdapter struct{ err error }

func (f failingSTTAdapter) Recognize(context.Context, audio.Frame, ...stt.STTOption) (stt.STTResult, error) {
	return stt.STTResult{}, f.err
}

type okSTTAdapter struct{ text string }

func (o okSTTAdapter) Recognize(context.Context, audio.Frame, ...stt.STTOption) (stt.STTResult, error) {
	return stt.STTResult{Text: o.text, IsFinal: true}, nil
}

type failingTTSAdapter struct{ err error }

func (f failingTTSAdapter) Synthesize(context.Context, string, ...tts.TTSOption) (io.ReadCloser, error) {
	return nil, f.err
}

func (f failingTTSAdapter) Voices(context.Context) ([]tts.Voice, error) { return nil, f.err }

type failingStreamSTTAdapter struct{ err error }

func (f failingStreamSTTAdapter) Recognize(context.Context, audio.Frame, ...stt.STTOption) (stt.STTResult, error) {
	return stt.STTResult{}, f.err
}

func (f failingStreamSTTAdapter) RecognizeStream(context.Context, audio.Stream[audio.Frame], ...stt.STTOption) (audio.Stream[stt.STTResult], error) {
	return nil, f.err
}

type okStreamSTTAdapter struct{ text string }

func (o okStreamSTTAdapter) Recognize(context.Context, audio.Frame, ...stt.STTOption) (stt.STTResult, error) {
	return stt.STTResult{Text: o.text, IsFinal: true}, nil
}

func (o okStreamSTTAdapter) RecognizeStream(context.Context, audio.Stream[audio.Frame], ...stt.STTOption) (audio.Stream[stt.STTResult], error) {
	out := audio.NewPipe[stt.STTResult](1)
	go func() {
		defer out.Close()
		out.Send(stt.STTResult{Text: o.text, IsFinal: true})
	}()
	return out, nil
}

type failingStreamTTSAdapter struct{ err error }

func (f failingStreamTTSAdapter) Synthesize(context.Context, string, ...tts.TTSOption) (io.ReadCloser, error) {
	return nil, f.err
}

func (f failingStreamTTSAdapter) Voices(context.Context) ([]tts.Voice, error) { return nil, f.err }

func (f failingStreamTTSAdapter) SynthesizeStream(context.Context, audio.Stream[string], ...tts.TTSOption) (audio.Stream[tts.Utterance], error) {
	return nil, f.err
}

type okStreamTTSAdapter struct{ payload string }

func (o okStreamTTSAdapter) Synthesize(context.Context, string, ...tts.TTSOption) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader([]byte(o.payload))), nil
}

func (o okStreamTTSAdapter) Voices(context.Context) ([]tts.Voice, error) { return nil, nil }

func (o okStreamTTSAdapter) SynthesizeStream(context.Context, audio.Stream[string], ...tts.TTSOption) (audio.Stream[tts.Utterance], error) {
	out := audio.NewPipe[tts.Utterance](1)
	go func() {
		defer out.Close()
		out.Send(tts.Utterance{Frame: audio.Frame{Data: []byte(o.payload)}, Text: "hello"})
	}()
	return out, nil
}

type slowSTT struct {
	delay  time.Duration
	result stt.STTResult
}

func (s *slowSTT) Recognize(ctx context.Context, input audio.Frame, opts ...stt.STTOption) (stt.STTResult, error) {
	select {
	case <-time.After(s.delay):
		if s.result.Audio.Data == nil {
			s.result.Audio = input
		}
		return s.result, nil
	case <-ctx.Done():
		return stt.STTResult{}, ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// Fake workflow.Runtime / workflow.Agent
// ---------------------------------------------------------------------------

type fakeAgent struct{}

func (fakeAgent) ID() string                  { return "test" }
func (fakeAgent) Card() workflow.AgentCard    { return workflow.AgentCard{} }
func (fakeAgent) Strategy() workflow.Strategy { return nil }
func (fakeAgent) Tools() []string             { return nil }

// fakeRuntime emits StreamEvents via the StreamCallback then returns.
type fakeRuntime struct {
	tokens     []string
	toolEvents []map[string]any
}

func (r *fakeRuntime) Run(ctx context.Context, agent workflow.Agent, req *workflow.Request, opts ...workflow.RunOption) (*workflow.Result, error) {
	rc := workflow.ApplyRunOpts(opts)

	tokens := r.tokens
	if len(tokens) == 0 {
		tokens = []string{"Hello ", "world."}
	}
	if rc.StreamCallback != nil {
		for _, tok := range tokens {
			rc.StreamCallback(workflow.StreamEvent{
				Type:    "token",
				Payload: map[string]any{"content": tok},
			})
		}
		for _, p := range r.toolEvents {
			evType, _ := p["type"].(string)
			rc.StreamCallback(workflow.StreamEvent{
				Type:    evType,
				Payload: p,
			})
		}
	}
	return &workflow.Result{}, nil
}

// errRuntime always returns an error.
type errRuntime struct{ err error }

func (r *errRuntime) Run(ctx context.Context, _ workflow.Agent, _ *workflow.Request, opts ...workflow.RunOption) (*workflow.Result, error) {
	return nil, r.err
}

// blockingRuntime blocks until ctx is cancelled (for Abort testing).
type blockingRuntime struct {
	running atomic.Bool
}

func (r *blockingRuntime) Run(ctx context.Context, _ workflow.Agent, _ *workflow.Request, opts ...workflow.RunOption) (*workflow.Result, error) {
	r.running.Store(true)
	<-ctx.Done()
	return nil, ctx.Err()
}

// slowRuntime does not emit any tokens for the given delay, for timeout testing.
type slowRuntime struct {
	delay time.Duration
}

func (r *slowRuntime) Run(ctx context.Context, _ workflow.Agent, _ *workflow.Request, opts ...workflow.RunOption) (*workflow.Result, error) {
	select {
	case <-time.After(r.delay):
		return &workflow.Result{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestPipeline_RunAudio_Basic(t *testing.T) {
	p := voice.NewPipeline(
		&fakeSTT{preset: "hello"},
		&fakeTTS{},
		&fakeRuntime{},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	ctx := context.Background()
	stream, err := p.RunAudio(ctx, audio.Frame{Data: []byte("wav")})
	if err != nil {
		t.Fatalf("RunAudio: %v", err)
	}

	events := collectStreamEvents(t, stream)

	var hasTranscriptFinal, hasTextDelta, hasAudio, hasDone bool
	for _, ev := range events {
		switch ev.Type {
		case voice.EventTranscriptFinal:
			hasTranscriptFinal = true
		case voice.EventTextDelta:
			hasTextDelta = true
		case voice.EventAudio:
			hasAudio = true
		case voice.EventDone:
			hasDone = true
		}
	}

	if !hasTranscriptFinal {
		t.Error("expected EventTranscriptFinal")
	}
	if !hasTextDelta {
		t.Error("expected EventTextDelta")
	}
	if !hasAudio {
		t.Error("expected EventAudio")
	}
	if !hasDone {
		t.Error("expected EventDone")
	}
	if len(events) > 0 && events[0].Type != voice.EventTranscriptFinal {
		t.Errorf("first event should be EventTranscriptFinal, got %s", events[0].Type)
	}
	if len(events) > 0 && events[len(events)-1].Type != voice.EventDone {
		t.Errorf("last event should be EventDone, got %s", events[len(events)-1].Type)
	}
}

func TestPipeline_RunAudio_EmptyTranscript(t *testing.T) {
	p := voice.NewPipeline(
		&fakeSTT{preset: ""},
		&fakeTTS{},
		&fakeRuntime{},
		fakeAgent{},
	)

	ctx := context.Background()
	stream, err := p.RunAudio(ctx, audio.Frame{Data: []byte("silence")})
	if err != nil {
		t.Fatalf("RunAudio: %v", err)
	}

	events := collectStreamEvents(t, stream)

	if len(events) < 2 {
		t.Fatalf("expected at least EventTranscriptFinal + EventDone, got %d events", len(events))
	}
	hasTranscriptFinal := false
	hasDone := false
	otherCount := 0
	for _, ev := range events {
		switch ev.Type {
		case voice.EventTranscriptFinal:
			hasTranscriptFinal = true
		case voice.EventDone:
			hasDone = true
		default:
			otherCount++
		}
	}
	if !hasTranscriptFinal {
		t.Error("expected EventTranscriptFinal")
	}
	if !hasDone {
		t.Error("expected EventDone")
	}
	if otherCount > 0 {
		t.Errorf("expected no other events, got %d", otherCount)
	}
}

func TestPipeline_RunAudioStream_WithStreamSTT(t *testing.T) {
	p := voice.NewPipeline(
		&fakeStreamSTT{preset: "hello"},
		&fakeTTS{},
		&fakeRuntime{},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	input := audio.NewPipe[audio.Frame](8)
	input.Send(audio.Frame{Data: []byte("wav")})
	input.Close()

	ctx := context.Background()
	stream, err := p.RunAudioStream(ctx, input)
	if err != nil {
		t.Fatalf("RunAudioStream: %v", err)
	}

	events := collectStreamEvents(t, stream)

	var hasTranscriptFinal, hasTextDelta, hasAudio, hasDone bool
	for _, ev := range events {
		switch ev.Type {
		case voice.EventTranscriptFinal:
			hasTranscriptFinal = true
		case voice.EventTextDelta:
			hasTextDelta = true
		case voice.EventAudio:
			hasAudio = true
		case voice.EventDone:
			hasDone = true
		}
	}

	if !hasTranscriptFinal {
		t.Error("expected EventTranscriptFinal")
	}
	if !hasTextDelta {
		t.Error("expected EventTextDelta")
	}
	if !hasAudio {
		t.Error("expected EventAudio")
	}
	if !hasDone {
		t.Error("expected EventDone")
	}
}

func TestPipeline_RunAudioStream_PreservesSTTTailAfterFirstFinal(t *testing.T) {
	p := voice.NewPipeline(
		tailingStreamSTT{},
		&fakeTTS{},
		&fakeRuntime{},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	input := audio.NewPipe[audio.Frame](1)
	input.Send(audio.Frame{Data: []byte("wav")})
	input.Close()

	stream, err := p.RunAudioStream(context.Background(), input)
	if err != nil {
		t.Fatalf("RunAudioStream: %v", err)
	}

	events := collectStreamEvents(t, stream)
	var sawTailPartial, sawTailFinal bool
	for _, ev := range events {
		if ev.Type == voice.EventTranscriptPartial && ev.Text == "hello wor" {
			sawTailPartial = true
		}
		if ev.Type == voice.EventTranscriptFinal && ev.Text == "hello world" {
			sawTailFinal = true
		}
	}

	if !sawTailPartial || !sawTailFinal {
		t.Fatalf("expected STT tail events after first final, got events: %+v", events)
	}
}

func TestPipeline_RunAudioStream_WithNonStreamSTT(t *testing.T) {
	p := voice.NewPipeline(
		&fakeSTT{preset: "hello"},
		&fakeTTS{},
		&fakeRuntime{},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	input := audio.NewPipe[audio.Frame](8)
	input.Send(audio.Frame{Data: []byte("wav")})
	input.Close()

	ctx := context.Background()
	stream, err := p.RunAudioStream(ctx, input)
	if err != nil {
		t.Fatalf("RunAudioStream: %v", err)
	}

	events := collectStreamEvents(t, stream)

	var hasTranscriptFinal, hasAudio, hasDone bool
	for _, ev := range events {
		switch ev.Type {
		case voice.EventTranscriptFinal:
			hasTranscriptFinal = true
		case voice.EventAudio:
			hasAudio = true
		case voice.EventDone:
			hasDone = true
		}
	}

	if !hasTranscriptFinal {
		t.Error("expected EventTranscriptFinal")
	}
	if !hasAudio {
		t.Error("expected EventAudio")
	}
	if !hasDone {
		t.Error("expected EventDone")
	}
}

func TestPipeline_RunAudioStream_WithStreamTTS(t *testing.T) {
	p := voice.NewPipeline(
		&fakeSTT{preset: "hello"},
		&fakeStreamTTS{fakeTTS: &fakeTTS{}},
		&fakeRuntime{},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	ctx := context.Background()
	stream, err := p.RunAudio(ctx, audio.Frame{Data: []byte("wav")})
	if err != nil {
		t.Fatalf("RunAudio: %v", err)
	}

	events := collectStreamEvents(t, stream)

	var hasAudio bool
	for _, ev := range events {
		if ev.Type == voice.EventAudio {
			hasAudio = true
			if !bytes.Contains(ev.Audio.Data, []byte("audio:")) {
				t.Errorf("EventAudio should contain 'audio:' prefix, got %q", string(ev.Audio.Data))
			}
			break
		}
	}
	if !hasAudio {
		t.Error("expected EventAudio")
	}
}

func TestPipeline_RunnerError(t *testing.T) {
	p := voice.NewPipeline(
		&fakeSTT{preset: "hello"},
		&fakeTTS{},
		&errRuntime{err: io.ErrClosedPipe},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	ctx := context.Background()
	stream, err := p.RunAudio(ctx, audio.Frame{Data: []byte("wav")})
	if err != nil {
		t.Fatalf("RunAudio: %v", err)
	}

	events := collectStreamEvents(t, stream)

	var hasError bool
	for _, ev := range events {
		if ev.Type == voice.EventError {
			hasError = true
			if ev.ErrorCode != voice.ErrorCodeTransport {
				t.Fatalf("EventError.ErrorCode = %q, want %q", ev.ErrorCode, voice.ErrorCodeTransport)
			}
			break
		}
	}
	if !hasError {
		t.Error("expected EventError")
	}
}

func TestPipeline_RunAudio_STTFinalTimeout(t *testing.T) {
	p := voice.NewPipeline(
		&slowSTT{
			delay:  200 * time.Millisecond,
			result: stt.STTResult{Text: "hello", IsFinal: true},
		},
		&fakeTTS{},
		&fakeRuntime{},
		fakeAgent{},
		voice.WithTimeouts(voice.PipelineTimeouts{
			STTFinal: 20 * time.Millisecond,
		}),
	)

	stream, err := p.RunAudio(context.Background(), audio.Frame{Data: []byte("wav")})
	if err != nil {
		t.Fatalf("RunAudio: %v", err)
	}

	events := collectStreamEvents(t, stream)
	for _, ev := range events {
		if ev.Type == voice.EventError {
			if ev.ErrorCode != voice.ErrorCodeTimeout {
				t.Fatalf("EventError.ErrorCode = %q, want %q", ev.ErrorCode, voice.ErrorCodeTimeout)
			}
			if !strings.Contains(ev.Text, "context deadline exceeded") {
				t.Fatalf("EventError.Text = %q, want context deadline exceeded", ev.Text)
			}
			return
		}
	}
	t.Fatalf("expected timeout EventError, got %+v", events)
}

func TestPipeline_RunText_RunnerFirstTokenTimeout(t *testing.T) {
	p := voice.NewPipeline(
		nil,
		&fakeTTS{},
		&slowRuntime{delay: 2 * time.Second},
		fakeAgent{},
		voice.WithTimeouts(voice.PipelineTimeouts{
			RunnerFirstToken: 20 * time.Millisecond,
		}),
	)

	stream, err := p.RunText(context.Background(), "hello")
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}

	events := collectStreamEvents(t, stream)
	for _, ev := range events {
		if ev.Type == voice.EventError {
			if ev.ErrorCode != voice.ErrorCodeTimeout {
				t.Fatalf("EventError.ErrorCode = %q, want %q", ev.ErrorCode, voice.ErrorCodeTimeout)
			}
			if !strings.Contains(ev.Text, "runner first token timeout") {
				t.Fatalf("EventError.Text = %q, want runner first token timeout", ev.Text)
			}
			return
		}
	}
	t.Fatalf("expected runner timeout EventError, got %+v", events)
}

func TestPipeline_RunAudio_AttachesSTTProviderReport(t *testing.T) {
	fallbackSTT := stt.NewFallbackSTT(
		failingSTTAdapter{err: io.ErrClosedPipe},
		okSTTAdapter{text: "hello"},
	)
	p := voice.NewPipeline(
		fallbackSTT,
		&fakeTTS{},
		&fakeRuntime{},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	stream, err := p.RunAudio(context.Background(), audio.Frame{Data: []byte("wav")})
	if err != nil {
		t.Fatalf("RunAudio: %v", err)
	}

	for _, ev := range collectStreamEvents(t, stream) {
		if ev.Type != voice.EventTranscriptFinal {
			continue
		}
		reportAny, ok := ev.Data["provider_report"]
		if !ok {
			t.Fatalf("expected provider_report in transcript event: %+v", ev)
		}
		report, ok := reportAny.(provider.Report)
		if !ok {
			t.Fatalf("provider_report type = %T, want provider.Report", reportAny)
		}
		if report.SelectedProvider == "" || len(report.Attempts) < 2 || !report.FallbackUsed {
			t.Fatalf("unexpected stt provider report: %+v", report)
		}
		return
	}
	t.Fatal("expected transcript final event")
}

func TestPipeline_RunAudio_AttachesTTSProviderReport(t *testing.T) {
	fallbackTTS := tts.NewFallbackTTS(
		failingStreamTTSAdapter{err: io.ErrClosedPipe},
		okStreamTTSAdapter{payload: "audio"},
	)
	p := voice.NewPipeline(
		&fakeSTT{preset: "hello"},
		fallbackTTS,
		&fakeRuntime{},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	stream, err := p.RunAudio(context.Background(), audio.Frame{Data: []byte("wav")})
	if err != nil {
		t.Fatalf("RunAudio: %v", err)
	}

	for _, ev := range collectStreamEvents(t, stream) {
		if ev.Type != voice.EventAudio {
			continue
		}
		reportAny, ok := ev.Data["provider_report"]
		if !ok {
			t.Fatalf("expected provider_report in audio event: %+v", ev)
		}
		report, ok := reportAny.(provider.Report)
		if !ok {
			t.Fatalf("provider_report type = %T, want provider.Report", reportAny)
		}
		if report.SelectedProvider == "" || len(report.Attempts) < 2 || !report.FallbackUsed {
			t.Fatalf("unexpected tts provider report: %+v", report)
		}
		return
	}
	t.Fatal("expected audio event")
}

func TestPipeline_RunAudio_TranscriptMetadata(t *testing.T) {
	p := voice.NewPipeline(
		metadataSTT{},
		&fakeTTS{},
		&fakeRuntime{},
		fakeAgent{},
	)

	stream, err := p.RunAudio(context.Background(), audio.Frame{Data: []byte("wav")})
	if err != nil {
		t.Fatalf("RunAudio: %v", err)
	}

	for _, ev := range collectStreamEvents(t, stream) {
		if ev.Type != voice.EventTranscriptFinal {
			continue
		}
		if ev.Lang != "en-US" || ev.Confidence != 0.92 || ev.Duration != 1500*time.Millisecond {
			t.Fatalf("unexpected transcript metadata: %+v", ev)
		}
		if ev.TranscriptRevision != 1 {
			t.Fatalf("TranscriptRevision=%d, want 1", ev.TranscriptRevision)
		}
		if len(ev.Words) != 1 || ev.Words[0].Word != "hello" {
			t.Fatalf("unexpected transcript words: %+v", ev.Words)
		}
		return
	}
	t.Fatal("expected transcript final event")
}

func TestPipeline_RunAudioStream_TranscriptRevisionIncrements(t *testing.T) {
	p := voice.NewPipeline(
		metadataStreamSTT{},
		&fakeTTS{},
		&fakeRuntime{},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	input := audio.NewPipe[audio.Frame](1)
	input.Send(audio.Frame{Data: []byte("wav")})
	input.Close()

	stream, err := p.RunAudioStream(context.Background(), input)
	if err != nil {
		t.Fatalf("RunAudioStream: %v", err)
	}

	var partial, final *voice.Event
	var revisions []voice.Event
	events := collectStreamEvents(t, stream)
	for i := range events {
		switch events[i].Type {
		case voice.EventTranscriptRevision:
			revisions = append(revisions, events[i])
		case voice.EventTranscriptPartial:
			partial = &events[i]
		case voice.EventTranscriptFinal:
			final = &events[i]
		}
	}
	if partial == nil || final == nil {
		t.Fatalf("expected partial and final transcript events, got %+v", events)
	}
	if len(revisions) != 2 {
		t.Fatalf("expected 2 transcript revision events, got %+v", revisions)
	}
	if revisions[0].TranscriptRevision != 1 || revisions[1].TranscriptRevision != 2 {
		t.Fatalf("unexpected revision event sequence: %+v", revisions)
	}
	if partial.TranscriptRevision != 1 || final.TranscriptRevision != 2 {
		t.Fatalf("unexpected transcript revisions: partial=%d final=%d", partial.TranscriptRevision, final.TranscriptRevision)
	}
	if final.Lang != "en-US" || final.Duration != 1200*time.Millisecond || len(final.Words) != 1 {
		t.Fatalf("unexpected final transcript metadata: %+v", *final)
	}
}

func TestPipeline_TranscriptAudio(t *testing.T) {
	audioData := []byte("original-audio-bytes")
	p := voice.NewPipeline(
		&fakeSTT{preset: "hello"},
		&fakeTTS{},
		&fakeRuntime{},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	ctx := context.Background()
	stream, err := p.RunAudio(ctx, audio.Frame{Data: audioData})
	if err != nil {
		t.Fatalf("RunAudio: %v", err)
	}

	events := collectStreamEvents(t, stream)

	for _, ev := range events {
		if ev.Type == voice.EventTranscriptFinal {
			if !bytes.Equal(ev.Audio.Data, audioData) {
				t.Errorf("EventTranscriptFinal.Audio = %q, want %q", ev.Audio.Data, audioData)
			}
			return
		}
	}
	t.Error("expected EventTranscriptFinal with Audio")
}

func TestPipeline_ToolCallEvents(t *testing.T) {
	toolRuntime := &fakeRuntime{
		tokens: []string{"Use ", "the ", "tool."},
		toolEvents: []map[string]any{
			{"type": "tool_call", "name": "get_weather"},
			{"type": "tool_result", "content": "sunny"},
		},
	}

	p := voice.NewPipeline(
		&fakeSTT{preset: "hello"},
		&fakeTTS{},
		toolRuntime,
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	ctx := context.Background()
	stream, err := p.RunAudio(ctx, audio.Frame{Data: []byte("wav")})
	if err != nil {
		t.Fatalf("RunAudio: %v", err)
	}

	events := collectStreamEvents(t, stream)

	var hasToolCall, hasToolResult bool
	for _, ev := range events {
		switch ev.Type {
		case voice.EventToolCall:
			hasToolCall = true
			if ev.Text != "get_weather" {
				t.Errorf("EventToolCall.Text = %q, want get_weather", ev.Text)
			}
		case voice.EventToolResult:
			hasToolResult = true
			if ev.Text != "sunny" {
				t.Errorf("EventToolResult.Text = %q, want sunny", ev.Text)
			}
		}
	}
	if !hasToolCall {
		t.Error("expected EventToolCall")
	}
	if !hasToolResult {
		t.Error("expected EventToolResult")
	}
}

func TestPipeline_Abort(t *testing.T) {
	rt := &blockingRuntime{}
	p := voice.NewPipeline(
		nil,
		&fakeTTS{},
		rt,
		fakeAgent{},
	)

	ctx := context.Background()
	stream, err := p.RunText(ctx, "hello")
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}

	// Wait for runtime to start running.
	for i := 0; i < 100; i++ {
		if rt.running.Load() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !rt.running.Load() {
		t.Fatal("runtime did not start")
	}

	if !p.Abort() {
		t.Error("Abort: expected true")
	}

	// Drain the stream to make sure it finishes.
	collectStreamEvents(t, stream)
}

func TestPipeline_NonStreamTTS(t *testing.T) {
	p := voice.NewPipeline(
		&fakeSTT{preset: "hello"},
		&fakeTTS{},
		&fakeRuntime{},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	ctx := context.Background()
	stream, err := p.RunAudio(ctx, audio.Frame{Data: []byte("wav")})
	if err != nil {
		t.Fatalf("RunAudio: %v", err)
	}

	events := collectStreamEvents(t, stream)

	var hasAudio bool
	for _, ev := range events {
		if ev.Type == voice.EventAudio {
			hasAudio = true
			if !bytes.HasPrefix(ev.Audio.Data, []byte("audio:")) {
				t.Errorf("EventAudio.Data = %q, want prefix 'audio:'", string(ev.Audio.Data))
			}
			break
		}
	}
	if !hasAudio {
		t.Error("expected EventAudio with non-stream TTS")
	}
}

func TestPipeline_RunText_Basic(t *testing.T) {
	p := voice.NewPipeline(
		nil,
		&fakeTTS{},
		&fakeRuntime{},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	ctx := context.Background()
	stream, err := p.RunText(ctx, "hello from text")
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}

	events := collectStreamEvents(t, stream)

	var hasTranscriptFinal, hasTextDelta, hasAudio, hasDone bool
	for _, ev := range events {
		switch ev.Type {
		case voice.EventTranscriptFinal:
			hasTranscriptFinal = true
			if ev.Text != "hello from text" {
				t.Errorf("EventTranscriptFinal.Text = %q, want %q", ev.Text, "hello from text")
			}
		case voice.EventTextDelta:
			hasTextDelta = true
		case voice.EventAudio:
			hasAudio = true
		case voice.EventDone:
			hasDone = true
		}
	}

	if !hasTranscriptFinal {
		t.Error("expected EventTranscriptFinal")
	}
	if !hasTextDelta {
		t.Error("expected EventTextDelta")
	}
	if !hasAudio {
		t.Error("expected EventAudio")
	}
	if !hasDone {
		t.Error("expected EventDone")
	}
	if len(events) > 0 && events[0].Type != voice.EventTranscriptFinal {
		t.Errorf("first event should be EventTranscriptFinal, got %s", events[0].Type)
	}
}

func TestPipeline_RunText_EmptyText(t *testing.T) {
	p := voice.NewPipeline(nil, &fakeTTS{}, &fakeRuntime{}, fakeAgent{})

	ctx := context.Background()
	stream, err := p.RunText(ctx, "")
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}

	events := collectStreamEvents(t, stream)

	var hasTranscriptFinal, hasDone bool
	otherCount := 0
	for _, ev := range events {
		switch ev.Type {
		case voice.EventTranscriptFinal:
			hasTranscriptFinal = true
		case voice.EventDone:
			hasDone = true
		default:
			otherCount++
		}
	}
	if !hasTranscriptFinal {
		t.Error("expected EventTranscriptFinal")
	}
	if !hasDone {
		t.Error("expected EventDone")
	}
	if otherCount > 0 {
		t.Errorf("expected no other events for empty text, got %d", otherCount)
	}
}

func TestPipeline_RunAudio_NilSTT(t *testing.T) {
	p := voice.NewPipeline(nil, &fakeTTS{}, &fakeRuntime{}, fakeAgent{})

	_, err := p.RunAudio(context.Background(), audio.Frame{Data: []byte("wav")})
	if err == nil {
		t.Fatal("expected error from RunAudio with nil STT")
	}
}

func TestPipeline_RunAudioStream_NilSTT(t *testing.T) {
	p := voice.NewPipeline(nil, &fakeTTS{}, &fakeRuntime{}, fakeAgent{})

	input := audio.NewPipe[audio.Frame](8)
	input.Close()

	_, err := p.RunAudioStream(context.Background(), input)
	if err == nil {
		t.Fatal("expected error from RunAudioStream with nil STT")
	}
}

func TestPipeline_InputInterruptClosesOutput(t *testing.T) {
	input := audio.NewPipe[audio.Frame](8)
	input.Send(audio.Frame{Data: []byte("wav")})

	p := voice.NewPipeline(
		&fakeStreamSTT{preset: "hello"},
		&fakeTTS{},
		&fakeRuntime{},
		fakeAgent{},
	)

	ctx := context.Background()
	stream, err := p.RunAudioStream(ctx, input)
	if err != nil {
		t.Fatalf("RunAudioStream: %v", err)
	}

	input.Interrupt()

	var lastErr error
	for {
		_, err := stream.Read()
		if err != nil {
			lastErr = err
			break
		}
	}

	if lastErr == nil {
		t.Error("expected error or EOF when reading from output after input interrupt")
	}
}

func TestPipeline_RunText_NonStreamTTS_SynthesizeError_EmitsErrorEvent(t *testing.T) {
	synthErr := fmt.Errorf("tts synthesis failed")
	p := voice.NewPipeline(
		nil,
		failingTTSAdapter{err: synthErr},
		&fakeRuntime{tokens: []string{"Hello."}},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	stream, err := p.RunText(context.Background(), "hello")
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}

	events := collectStreamEvents(t, stream)
	var gotErrorEvent bool
	for _, ev := range events {
		if ev.Type == voice.EventError && strings.Contains(ev.Text, "tts synthesis failed") {
			gotErrorEvent = true
		}
	}
	if !gotErrorEvent {
		types := make([]voice.EventType, len(events))
		for i, ev := range events {
			types[i] = ev.Type
		}
		t.Fatalf("expected error event for TTS synthesis failure, got events: %v", types)
	}
}

func collectTextDeltas(events []voice.Event) string {
	var sb strings.Builder
	for _, ev := range events {
		if ev.Type == voice.EventTextDelta {
			sb.WriteString(ev.Text)
		}
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// eventCh overflow (S-4)
// ---------------------------------------------------------------------------

// overflowRuntime emits a burst of tokens synchronously.
type overflowRuntime struct {
	count int
}

func (r *overflowRuntime) Run(ctx context.Context, _ workflow.Agent, _ *workflow.Request, opts ...workflow.RunOption) (*workflow.Result, error) {
	rc := workflow.ApplyRunOpts(opts)
	if rc.StreamCallback != nil {
		for i := 0; i < r.count; i++ {
			rc.StreamCallback(workflow.StreamEvent{
				Type:    "token",
				Payload: map[string]any{"content": "x"},
			})
		}
	}
	return &workflow.Result{}, nil
}

func TestPipeline_RunText_EventOverflowWarning(t *testing.T) {
	p := voice.NewPipeline(
		nil,
		&fakeTTS{},
		&overflowRuntime{count: 2000},
		fakeAgent{},
	)

	stream, err := p.RunText(context.Background(), "hello")
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}

	events := collectStreamEvents(t, stream)

	var tokenCount int
	var overflowEvent *voice.Event
	for i, ev := range events {
		if ev.Type == voice.EventTextDelta {
			tokenCount++
		}
		if ev.Type == voice.EventError && ev.ErrorCode == voice.ErrorCodeInternal &&
			strings.Contains(ev.Text, "dropped") {
			overflowEvent = &events[i]
		}
	}

	if tokenCount == 2000 {
		t.Skip("no overflow occurred (consumer was fast enough), skipping")
	}

	if overflowEvent == nil {
		t.Fatalf("tokens dropped (only %d of 2000 received) but no overflow warning event", tokenCount)
	}
}

// ---------------------------------------------------------------------------
// Warmup lifecycle (S-5)
// ---------------------------------------------------------------------------

type warmupTTS struct {
	*fakeTTS
	called atomic.Bool
	done   chan struct{}
}

func (w *warmupTTS) Warmup(ctx context.Context) error {
	w.called.Store(true)
	<-ctx.Done()
	close(w.done)
	return ctx.Err()
}

func TestPipeline_RunText_WarmupLifecycle(t *testing.T) {
	wt := &warmupTTS{fakeTTS: &fakeTTS{}, done: make(chan struct{})}
	p := voice.NewPipeline(
		nil,
		wt,
		&fakeRuntime{},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	stream, err := p.RunText(context.Background(), "hello")
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}
	collectStreamEvents(t, stream)

	if !wt.called.Load() {
		t.Fatal("Warmup was not called")
	}

	select {
	case <-wt.done:
	case <-time.After(2 * time.Second):
		t.Fatal("warmup goroutine was not cancelled after pipeline completed")
	}
}

// ---------------------------------------------------------------------------
// P1 refactoring: startFlow + runTTS split coverage
// ---------------------------------------------------------------------------

func TestPipeline_EventOrdering_Strict(t *testing.T) {
	p := voice.NewPipeline(
		&fakeSTT{preset: "hello"},
		&fakeTTS{},
		&fakeRuntime{},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	stream, err := p.RunAudio(context.Background(), audio.Frame{Data: []byte("wav")})
	if err != nil {
		t.Fatalf("RunAudio: %v", err)
	}
	events := collectStreamEvents(t, stream)

	lastTextDelta := -1
	firstResponseDone := -1
	firstAudioDone := -1
	firstDone := -1

	for i, ev := range events {
		switch ev.Type {
		case voice.EventTextDelta:
			lastTextDelta = i
		case voice.EventResponseDone:
			if firstResponseDone == -1 {
				firstResponseDone = i
			}
		case voice.EventAudioDone:
			if firstAudioDone == -1 {
				firstAudioDone = i
			}
		case voice.EventDone:
			if firstDone == -1 {
				firstDone = i
			}
		}
	}

	if lastTextDelta < 0 || firstResponseDone < 0 || firstAudioDone < 0 || firstDone < 0 {
		types := make([]voice.EventType, len(events))
		for i, ev := range events {
			types[i] = ev.Type
		}
		t.Fatalf("missing lifecycle events: %v", types)
	}
	if firstResponseDone < lastTextDelta {
		t.Errorf("EventResponseDone (idx %d) before last EventTextDelta (idx %d)", firstResponseDone, lastTextDelta)
	}
	if firstAudioDone < firstResponseDone {
		t.Errorf("EventAudioDone (idx %d) before EventResponseDone (idx %d)", firstAudioDone, firstResponseDone)
	}
	if firstDone < firstAudioDone {
		t.Errorf("EventDone (idx %d) before EventAudioDone (idx %d)", firstDone, firstAudioDone)
	}
	if firstDone != len(events)-1 {
		t.Errorf("EventDone should be last event, at idx %d of %d", firstDone, len(events))
	}
}

func TestPipeline_RunText_AllEventsShareRunID(t *testing.T) {
	p := voice.NewPipeline(
		nil,
		&fakeTTS{},
		&fakeRuntime{},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	stream, err := p.RunText(context.Background(), "hello")
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}
	events := collectStreamEvents(t, stream)

	var runID string
	for _, ev := range events {
		if ev.RunID == "" {
			continue
		}
		if runID == "" {
			runID = ev.RunID
		} else if ev.RunID != runID {
			t.Fatalf("inconsistent RunID: %q vs %q (event type: %s)", runID, ev.RunID, ev.Type)
		}
	}
	if runID == "" {
		t.Fatal("no events with RunID found")
	}
}

func TestPipeline_ContextCancel_TerminatesCleanly(t *testing.T) {
	rt := &blockingRuntime{}
	p := voice.NewPipeline(
		nil,
		&fakeTTS{},
		rt,
		fakeAgent{},
	)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := p.RunText(ctx, "hello")
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}

	for i := 0; i < 100; i++ {
		if rt.running.Load() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !rt.running.Load() {
		t.Fatal("runtime did not start")
	}

	cancel()

	done := make(chan struct{})
	go func() {
		collectStreamEvents(t, stream)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not terminate after context cancellation")
	}
}

func TestPipeline_RunAudioStream_ContextCancel_NoHang(t *testing.T) {
	p := voice.NewPipeline(
		&fakeStreamSTT{preset: "hello"},
		&fakeTTS{},
		&blockingRuntime{},
		fakeAgent{},
	)

	input := audio.NewPipe[audio.Frame](8)
	input.Send(audio.Frame{Data: []byte("wav")})
	input.Close()

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := p.RunAudioStream(ctx, input)
	if err != nil {
		t.Fatalf("RunAudioStream: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	done := make(chan struct{})
	go func() {
		collectStreamEvents(t, stream)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not terminate after context cancellation (RunAudioStream)")
	}
}

func TestPipeline_RunText_MultipleSentences(t *testing.T) {
	p := voice.NewPipeline(
		nil,
		&fakeTTS{},
		&fakeRuntime{tokens: []string{"First sentence.", " Second sentence."}},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	stream, err := p.RunText(context.Background(), "hello")
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}

	events := collectStreamEvents(t, stream)
	var audioCount int
	for _, ev := range events {
		if ev.Type == voice.EventAudio {
			audioCount++
		}
	}
	if audioCount < 2 {
		t.Fatalf("expected at least 2 audio events for 2 sentences, got %d", audioCount)
	}
}

func TestPipeline_Abort_EmitsLifecycleEvents(t *testing.T) {
	rt := &blockingRuntime{}
	p := voice.NewPipeline(
		nil,
		&fakeTTS{},
		rt,
		fakeAgent{},
	)

	stream, err := p.RunText(context.Background(), "hello")
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}

	for i := 0; i < 100; i++ {
		if rt.running.Load() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	p.Abort()

	events := collectStreamEvents(t, stream)

	var hasResponseDone, hasAudioDone, hasDone bool
	for _, ev := range events {
		switch ev.Type {
		case voice.EventResponseDone:
			hasResponseDone = true
		case voice.EventAudioDone:
			hasAudioDone = true
		case voice.EventDone:
			hasDone = true
		}
	}
	if !hasResponseDone {
		t.Error("expected EventResponseDone after Abort")
	}
	if !hasAudioDone {
		t.Error("expected EventAudioDone after Abort")
	}
	if !hasDone {
		t.Error("expected EventDone after Abort")
	}
}

// slowStreamTTS reads sentences but never produces utterances (for TTSFirstAudio timeout testing).
type slowStreamTTS struct{}

func (slowStreamTTS) Synthesize(context.Context, string, ...tts.TTSOption) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}

func (slowStreamTTS) Voices(context.Context) ([]tts.Voice, error) { return nil, nil }

func (slowStreamTTS) SynthesizeStream(ctx context.Context, input audio.Stream[string], opts ...tts.TTSOption) (audio.Stream[tts.Utterance], error) {
	out := audio.NewPipe[tts.Utterance](1)
	go func() {
		defer out.Close()
		for {
			_, err := input.Read()
			if err != nil {
				<-ctx.Done()
				return
			}
		}
	}()
	return out, nil
}

func TestPipeline_RunText_TTSFirstAudioTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := voice.NewPipeline(
		nil,
		slowStreamTTS{},
		&fakeRuntime{tokens: []string{"Hello."}},
		fakeAgent{},
		voice.WithTimeouts(voice.PipelineTimeouts{
			TTSFirstAudio: 50 * time.Millisecond,
		}),
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	stream, err := p.RunText(ctx, "hello")
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}

	events := collectStreamEvents(t, stream)
	for _, ev := range events {
		if ev.Type == voice.EventError && strings.Contains(ev.Text, "tts first audio timeout") {
			if ev.ErrorCode != voice.ErrorCodeTimeout {
				t.Fatalf("EventError.ErrorCode = %q, want %q", ev.ErrorCode, voice.ErrorCodeTimeout)
			}
			return
		}
	}
	types := make([]voice.EventType, len(events))
	for i, ev := range events {
		types[i] = ev.Type
	}
	t.Fatalf("expected TTS first audio timeout EventError, got events: %v", types)
}

func TestPipeline_EventOrdering_StreamTTS(t *testing.T) {
	p := voice.NewPipeline(
		&fakeSTT{preset: "hello"},
		&fakeStreamTTS{fakeTTS: &fakeTTS{}},
		&fakeRuntime{},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	stream, err := p.RunAudio(context.Background(), audio.Frame{Data: []byte("wav")})
	if err != nil {
		t.Fatalf("RunAudio: %v", err)
	}
	events := collectStreamEvents(t, stream)

	lastTextDelta := -1
	firstAudioDone := -1
	firstDone := -1

	for i, ev := range events {
		switch ev.Type {
		case voice.EventTextDelta:
			lastTextDelta = i
		case voice.EventAudioDone:
			if firstAudioDone == -1 {
				firstAudioDone = i
			}
		case voice.EventDone:
			if firstDone == -1 {
				firstDone = i
			}
		}
	}

	if lastTextDelta < 0 || firstAudioDone < 0 || firstDone < 0 {
		t.Fatalf("missing lifecycle events")
	}
	if firstAudioDone <= lastTextDelta {
		t.Errorf("EventAudioDone should come after last EventTextDelta")
	}
	if firstDone <= firstAudioDone {
		t.Errorf("EventDone should come after EventAudioDone")
	}
}

func TestPipeline_RunText_StreamTTS_EventOrdering(t *testing.T) {
	p := voice.NewPipeline(
		nil,
		&fakeStreamTTS{fakeTTS: &fakeTTS{}},
		&fakeRuntime{tokens: []string{"One.", " Two."}},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	stream, err := p.RunText(context.Background(), "hello")
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}
	events := collectStreamEvents(t, stream)

	var audioCount int
	lastAudio := -1
	firstAudioDone := -1
	firstDone := -1

	for i, ev := range events {
		switch ev.Type {
		case voice.EventAudio:
			audioCount++
			lastAudio = i
		case voice.EventAudioDone:
			if firstAudioDone == -1 {
				firstAudioDone = i
			}
		case voice.EventDone:
			if firstDone == -1 {
				firstDone = i
			}
		}
	}

	if audioCount < 2 {
		t.Fatalf("expected ≥2 audio events, got %d", audioCount)
	}
	if firstAudioDone <= lastAudio {
		t.Errorf("EventAudioDone should come after last EventAudio")
	}
	if firstDone <= firstAudioDone {
		t.Errorf("EventDone should come after EventAudioDone")
	}
}

func TestPipeline_RunAudio_WarmupLifecycle(t *testing.T) {
	wt := &warmupTTS{fakeTTS: &fakeTTS{}, done: make(chan struct{})}
	p := voice.NewPipeline(
		&fakeSTT{preset: "hello"},
		wt,
		&fakeRuntime{},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	stream, err := p.RunAudio(context.Background(), audio.Frame{Data: []byte("wav")})
	if err != nil {
		t.Fatalf("RunAudio: %v", err)
	}
	collectStreamEvents(t, stream)

	if !wt.called.Load() {
		t.Fatal("Warmup was not called")
	}

	select {
	case <-wt.done:
	case <-time.After(2 * time.Second):
		t.Fatal("warmup goroutine was not cancelled after pipeline completed")
	}
}
