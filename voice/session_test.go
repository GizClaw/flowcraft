package voice_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/voice"
	"github.com/GizClaw/flowcraft/voice/audio"
	"github.com/GizClaw/flowcraft/voice/detect"
	speechmetrics "github.com/GizClaw/flowcraft/voice/metrics"
	"github.com/GizClaw/flowcraft/voice/preprocess"
	"github.com/GizClaw/flowcraft/voice/stt"
	"github.com/GizClaw/flowcraft/voice/tts"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// Frame helpers for testing.
func makeSilentFrame(samples int) audio.Frame {
	return audio.Frame{
		Data:   make([]byte, samples*2),
		Format: audio.Format{Codec: audio.CodecPCM, SampleRate: 24000, Channels: 1, BitDepth: 16},
	}
}

func makeLoudFrame(samples int) audio.Frame {
	b := make([]byte, samples*2)
	for i := range samples {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(16384))
	}
	return audio.Frame{
		Data:   b,
		Format: audio.Format{Codec: audio.CodecPCM, SampleRate: 24000, Channels: 1, BitDepth: 16},
	}
}

// Samples per 100ms at 24kHz.
const samplesPerChunk = 2400

// fakeAudioSource wraps a Pipe[Frame]. Feeds frames then waits for ctx.Done.
type fakeAudioSource struct {
	pipe *audio.Pipe[audio.Frame]
}

func newFakeAudioSource(frames []audio.Frame, ctx context.Context) *fakeAudioSource {
	pipe := audio.NewPipe[audio.Frame](len(frames) + 1)
	go func() {
		for _, f := range frames {
			if !pipe.Send(f) {
				return
			}
		}
		// Keep sending silence (like a real microphone) until ctx is cancelled.
		silence := makeSilentFrame(samplesPerChunk)
		for {
			select {
			case <-ctx.Done():
				pipe.Interrupt()
				return
			default:
				if !pipe.Send(silence) {
					return
				}
			}
		}
	}()
	return &fakeAudioSource{pipe: pipe}
}

func (s *fakeAudioSource) Stream() audio.Stream[audio.Frame] { return s.pipe }

// fakeAudioSink records Play calls and drains the utterance stream.
type fakeAudioSink struct {
	mu    sync.Mutex
	plays int
}

func (s *fakeAudioSink) Play(stream audio.Stream[tts.Utterance]) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.mu.Lock()
		s.plays++
		s.mu.Unlock()
		for {
			_, err := stream.Read()
			if err != nil {
				return
			}
		}
	}()
	return done
}

func (s *fakeAudioSink) Plays() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.plays
}

type stickyAudioSink struct {
	drainDelay time.Duration
}

func (s *stickyAudioSink) Play(stream audio.Stream[tts.Utterance]) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for {
			_, err := stream.Read()
			if err != nil {
				return
			}
			if s.drainDelay > 0 {
				time.Sleep(s.drainDelay)
			}
		}
	}()
	return done
}

type countingProcessor struct {
	n atomic.Int32
}

func (p *countingProcessor) Process(frame audio.Frame) audio.Frame {
	p.n.Add(1)
	return frame
}

type capturingTTS struct {
	lastVoice   string
	lastSpeed   float64
	lastRate    int
	lastLang    string
	lastEmotion string
	lastScene   string
	lastVolume  float64
}

func (c *capturingTTS) Synthesize(ctx context.Context, text string, opts ...tts.TTSOption) (io.ReadCloser, error) {
	applied := tts.ApplyTTSOptions(opts...)
	c.lastVoice = applied.Voice
	c.lastSpeed = applied.Speed
	c.lastRate = applied.Rate
	c.lastLang = applied.ExtraString("speech.language", "")
	c.lastEmotion = applied.ExtraString("speech.emotion", "")
	c.lastScene = applied.ExtraString("speech.scene", "")
	c.lastVolume = applied.ExtraFloat64("speech.volume", 0)
	return io.NopCloser(bytes.NewReader([]byte("audio:" + text))), nil
}

func (c *capturingTTS) Voices(context.Context) ([]tts.Voice, error) { return nil, nil }

type pacedAudioSource struct {
	pipe *audio.Pipe[audio.Frame]
}

func newPacedAudioSource(ctx context.Context, frames []audio.Frame, gap time.Duration) *pacedAudioSource {
	pipe := audio.NewPipe[audio.Frame](len(frames) + 4)
	go func() {
		defer pipe.Close()
		for _, f := range frames {
			select {
			case <-ctx.Done():
				pipe.Interrupt()
				return
			default:
			}
			if !pipe.Send(f) {
				return
			}
			time.Sleep(gap)
		}
		<-ctx.Done()
	}()
	return &pacedAudioSource{pipe: pipe}
}

func (s *pacedAudioSource) Stream() audio.Stream[audio.Frame] { return s.pipe }

// delayedRuntime delays before completing, so pipeline stays in "responding" state
// long enough for barge-in frames to be processed.
type delayedRuntime struct {
	tokens   []string
	delay    time.Duration
	cancelMu sync.Mutex
	cancelFn context.CancelFunc
	abortCnt atomic.Int32
}

func newDelayedRuntime(tokens []string, delay time.Duration) *delayedRuntime {
	return &delayedRuntime{tokens: tokens, delay: delay}
}

func (r *delayedRuntime) Run(ctx context.Context, _ workflow.Agent, _ *workflow.Request, opts ...workflow.RunOption) (*workflow.Result, error) {
	rc := workflow.ApplyRunOpts(opts)

	tokens := r.tokens
	if len(tokens) == 0 {
		tokens = []string{"Hello "}
	}
	if rc.StreamCallback != nil {
		for _, tok := range tokens {
			rc.StreamCallback(workflow.StreamEvent{
				Type:    "token",
				Payload: map[string]any{"content": tok},
			})
		}
	}
	select {
	case <-time.After(r.delay):
		return &workflow.Result{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *delayedRuntime) AbortCount() int { return int(r.abortCnt.Load()) }

// staleTokenRuntime emits tokens on turn 1 with a pause (simulating a slow response),
// then fast tokens on turn 2.
type staleTokenRuntime struct {
	startCnt atomic.Int32
}

func (r *staleTokenRuntime) Run(ctx context.Context, _ workflow.Agent, _ *workflow.Request, opts ...workflow.RunOption) (*workflow.Result, error) {
	rc := workflow.ApplyRunOpts(opts)
	turn := r.startCnt.Add(1)

	if rc.StreamCallback != nil {
		switch turn {
		case 1:
			rc.StreamCallback(workflow.StreamEvent{
				Type:    "token",
				Payload: map[string]any{"content": "old-early "},
			})
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			rc.StreamCallback(workflow.StreamEvent{
				Type:    "token",
				Payload: map[string]any{"content": "old-late "},
			})
		default:
			time.Sleep(30 * time.Millisecond)
			rc.StreamCallback(workflow.StreamEvent{
				Type:    "token",
				Payload: map[string]any{"content": "new-turn "},
			})
		}
	}
	return &workflow.Result{}, nil
}

func newSessionPipeline(transcript string) *voice.Pipeline {
	var tokens []string
	if transcript != "" {
		tokens = []string{transcript}
	}
	return voice.NewPipeline(
		&fakeStreamSTT{preset: transcript},
		&fakeStreamTTS{fakeTTS: &fakeTTS{}},
		&fakeRuntime{tokens: tokens},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)
}

// newTextOnlyPipeline creates a pipeline with no STT (text-only mode).
func newTextOnlyPipeline() *voice.Pipeline {
	return voice.NewPipeline(
		nil,
		&fakeStreamTTS{fakeTTS: &fakeTTS{}},
		&fakeRuntime{tokens: []string{"reply"}},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)
}

func newSessionPipelineForBargeIn(transcript string) (*voice.Pipeline, *delayedRuntime) {
	var tokens []string
	if transcript != "" {
		tokens = []string{transcript}
	}
	rt := newDelayedRuntime(tokens, 500*time.Millisecond)
	pipeline := voice.NewPipeline(
		&fakeStreamSTT{preset: transcript},
		&fakeStreamTTS{fakeTTS: &fakeTTS{}},
		rt,
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)
	return pipeline, rt
}

func TestSession_SingleTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var frames []audio.Frame
	for i := 0; i < 5; i++ {
		frames = append(frames, makeLoudFrame(samplesPerChunk))
	}
	for i := 0; i < 8; i++ {
		frames = append(frames, makeSilentFrame(samplesPerChunk))
	}

	source := newFakeAudioSource(frames, ctx)
	sink := &fakeAudioSink{}
	pipeline := newSessionPipeline("hello")

	var events []voice.Event
	session := voice.NewSession(pipeline, source, sink,
		voice.WithSilenceDuration(700*time.Millisecond),
		voice.WithFrameSize(100*time.Millisecond),
		voice.WithEventHandler(func(ev voice.Event) {
			events = append(events, ev)
			if ev.Type == voice.EventDone {
				cancel()
			}
		}),
	)

	err := session.Run(ctx)
	if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		t.Fatalf("Run: %v", err)
	}

	if sink.Plays() < 1 {
		t.Errorf("expected at least 1 Play call, got %d", sink.Plays())
	}
	if len(events) == 0 {
		t.Error("expected at least one pipeline event")
	}
	hasDone := false
	for _, ev := range events {
		if ev.Type == voice.EventDone {
			hasDone = true
		}
	}
	if !hasDone {
		t.Error("expected EventDone (triggered by Sink playback completion)")
	}
}

func TestSession_BargeIn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pipeline, _ := newSessionPipelineForBargeIn("hi")

	var frames []audio.Frame
	for i := 0; i < 3; i++ {
		frames = append(frames, makeLoudFrame(samplesPerChunk))
	}
	for i := 0; i < 8; i++ {
		frames = append(frames, makeSilentFrame(samplesPerChunk))
	}
	for i := 0; i < 4; i++ {
		frames = append(frames, makeLoudFrame(samplesPerChunk))
	}
	for i := 0; i < 8; i++ {
		frames = append(frames, makeSilentFrame(samplesPerChunk))
	}

	source := newFakeAudioSource(frames, ctx)
	sink := &fakeAudioSink{}

	var interrupted bool
	var doneCount int
	session := voice.NewSession(pipeline, source, sink,
		voice.WithSilenceDuration(700*time.Millisecond),
		voice.WithFrameSize(100*time.Millisecond),
		voice.WithDetector(detect.NewEnergyDetector(
			detect.WithDetectorInterruptThreshold(0.015),
			detect.WithDetectorConfirm(3),
		)),
		voice.WithEventHandler(func(ev voice.Event) {
			if ev.Type == voice.EventTurnInterrupted {
				interrupted = true
			}
			if ev.Type == voice.EventDone {
				doneCount++
				if doneCount >= 1 {
					cancel()
				}
			}
		}),
	)

	err := session.Run(ctx)
	if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		t.Fatalf("Run: %v", err)
	}

	if !interrupted {
		t.Error("expected barge-in interrupt")
	}
}

func TestSession_BargeInTransientNoiseDoesNotInterrupt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pipeline, _ := newSessionPipelineForBargeIn("hi")

	var frames []audio.Frame
	for i := 0; i < 3; i++ {
		frames = append(frames, makeLoudFrame(samplesPerChunk))
	}
	for i := 0; i < 8; i++ {
		frames = append(frames, makeSilentFrame(samplesPerChunk))
	}
	for i := 0; i < 3; i++ {
		frames = append(frames, makeLoudFrame(samplesPerChunk))
	}
	for i := 0; i < 8; i++ {
		frames = append(frames, makeSilentFrame(samplesPerChunk))
	}

	source := newFakeAudioSource(frames, ctx)
	sink := &fakeAudioSink{}

	var interrupted bool
	session := voice.NewSession(pipeline, source, sink,
		voice.WithSilenceDuration(700*time.Millisecond),
		voice.WithFrameSize(100*time.Millisecond),
		voice.WithDetector(detect.NewEnergyDetector(
			detect.WithDetectorInterruptThreshold(0.015),
			detect.WithDetectorConfirm(3),
		)),
		voice.WithBargeInConfirm(2),
		voice.WithEventHandler(func(ev voice.Event) {
			if ev.Type == voice.EventTurnInterrupted {
				interrupted = true
			}
			if ev.Type == voice.EventDone {
				cancel()
			}
		}),
	)

	err := session.Run(ctx)
	if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		t.Fatalf("Run: %v", err)
	}

	if interrupted {
		t.Fatal("did not expect transient noise to trigger barge-in")
	}
}

func TestSession_PreprocessorChainAppliesToInputFrames(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	frames := []audio.Frame{makeSilentFrame(samplesPerChunk)}
	for i := 0; i < 8; i++ {
		frames = append(frames, makeSilentFrame(samplesPerChunk))
	}
	source := newFakeAudioSource(frames, ctx)
	sink := &fakeAudioSink{}
	pipeline := newSessionPipeline("hello")
	counter := &countingProcessor{}

	session := voice.NewSession(pipeline, source, sink,
		voice.WithSilenceDuration(700*time.Millisecond),
		voice.WithFrameSize(100*time.Millisecond),
		voice.WithPreprocessors(
			counter,
			preprocess.Func(func(frame audio.Frame) audio.Frame {
				return makeLoudFrame(samplesPerChunk)
			}),
		),
		voice.WithEventHandler(func(ev voice.Event) {
			if ev.Type == voice.EventDone {
				cancel()
			}
		}),
	)

	err := session.Run(ctx)
	if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		t.Fatalf("Run: %v", err)
	}

	if counter.n.Load() == 0 {
		t.Fatal("expected preprocessor chain to process input frames")
	}
	if sink.Plays() < 1 {
		t.Fatalf("expected preprocessed loud frame to start a turn, got Plays=%d", sink.Plays())
	}
}

func TestSession_VoiceProfileAppliesDynamicTTSOptions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ttsCapture := &capturingTTS{}
	pipeline := voice.NewPipeline(
		nil,
		ttsCapture,
		&fakeRuntime{tokens: []string{"reply"}},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)
	session := voice.NewSession(pipeline, nil, &fakeAudioSink{},
		voice.WithVoiceProfile(voice.VoiceProfile{
			Language: "zh-CN",
			Voice:    "xiaoyi",
			Speed:    1.15,
			Emotion:  "calm",
			Volume:   0.8,
			Rate:     22050,
			Scene:    voice.VoiceProfileSceneCompanion,
		}),
		voice.WithEventHandler(func(ev voice.Event) {
			if ev.Type == voice.EventDone {
				cancel()
			}
		}),
	)

	go func() {
		time.Sleep(20 * time.Millisecond)
		session.Send("hello")
	}()

	err := session.Run(ctx)
	if err != nil && err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}

	if ttsCapture.lastVoice != "xiaoyi" || ttsCapture.lastLang != "zh-CN" ||
		ttsCapture.lastEmotion != "calm" || ttsCapture.lastScene != string(voice.VoiceProfileSceneCompanion) {
		t.Fatalf("voice profile extras not applied: %+v", ttsCapture)
	}
	if ttsCapture.lastSpeed != 1.15 || ttsCapture.lastRate != 22050 || ttsCapture.lastVolume != 0.8 {
		t.Fatalf("voice profile numeric options not applied: %+v", ttsCapture)
	}
}

func TestSession_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	frames := []audio.Frame{makeLoudFrame(samplesPerChunk)}
	source := newFakeAudioSource(frames, ctx)
	sink := &fakeAudioSink{}
	pipeline := newSessionPipeline("x")

	session := voice.NewSession(pipeline, source, sink)

	err := session.Run(ctx)
	if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		t.Fatalf("Run: unexpected error %v", err)
	}
}

func TestSession_EmptySpeech(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var frames []audio.Frame
	for i := 0; i < 20; i++ {
		frames = append(frames, makeSilentFrame(samplesPerChunk))
	}

	source := newFakeAudioSource(frames, ctx)
	sink := &fakeAudioSink{}
	pipeline := newSessionPipeline("")

	var events []voice.Event
	session := voice.NewSession(pipeline, source, sink,
		voice.WithEventHandler(func(ev voice.Event) {
			events = append(events, ev)
		}),
	)

	err := session.Run(ctx)
	if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		t.Fatalf("Run: %v", err)
	}

	if sink.Plays() != 0 {
		t.Errorf("expected 0 Play calls for silence-only input, got %d", sink.Plays())
	}
	if len(events) != 0 {
		t.Errorf("expected no pipeline events for silence-only input, got %d", len(events))
	}
}

type fakeClassifier struct {
	threshold float64
	calls     int
}

func (c *fakeClassifier) Classify(chunk []byte) (float64, bool) {
	c.calls++
	if len(chunk) < 2 {
		return 0, false
	}
	var sumSq float64
	samples := len(chunk) / 2
	for i := 0; i < samples; i++ {
		s := int16(binary.LittleEndian.Uint16(chunk[i*2:]))
		n := float64(s) / 32768.0
		sumSq += n * n
	}
	rms := sumSq / float64(samples)
	return rms, rms >= c.threshold
}

func TestSession_WithClassifier(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var frames []audio.Frame
	for i := 0; i < 5; i++ {
		frames = append(frames, makeLoudFrame(samplesPerChunk))
	}
	for i := 0; i < 8; i++ {
		frames = append(frames, makeSilentFrame(samplesPerChunk))
	}

	source := newFakeAudioSource(frames, ctx)
	sink := &fakeAudioSink{}
	pipeline := newSessionPipeline("hello")
	classifier := &fakeClassifier{threshold: 0.0001}

	var gotDone bool
	session := voice.NewSession(pipeline, source, sink,
		voice.WithDetector(detect.NewEnergyDetector(
			detect.WithDetectorClassifier(classifier),
			detect.WithDetectorThreshold(0.01),
			detect.WithDetectorInterruptThreshold(0.05),
		)),
		voice.WithSilenceDuration(700*time.Millisecond),
		voice.WithFrameSize(100*time.Millisecond),
		voice.WithEventHandler(func(ev voice.Event) {
			if ev.Type == voice.EventDone {
				gotDone = true
				cancel()
			}
		}),
	)

	err := session.Run(ctx)
	if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		t.Fatalf("Run: %v", err)
	}

	if classifier.calls == 0 {
		t.Error("classifier was never called")
	}
	if !gotDone {
		t.Error("expected EventDone — session should work with classifier")
	}
	if sink.Plays() < 1 {
		t.Errorf("expected at least 1 Play call, got %d", sink.Plays())
	}
}

func TestSession_ClassifierBargeIn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pipeline, _ := newSessionPipelineForBargeIn("hi")
	classifier := &fakeClassifier{threshold: 0.0001}

	var frames []audio.Frame
	for i := 0; i < 3; i++ {
		frames = append(frames, makeLoudFrame(samplesPerChunk))
	}
	for i := 0; i < 8; i++ {
		frames = append(frames, makeSilentFrame(samplesPerChunk))
	}
	for i := 0; i < 4; i++ {
		frames = append(frames, makeLoudFrame(samplesPerChunk))
	}
	for i := 0; i < 8; i++ {
		frames = append(frames, makeSilentFrame(samplesPerChunk))
	}

	source := newFakeAudioSource(frames, ctx)
	sink := &fakeAudioSink{}

	var interrupted bool
	session := voice.NewSession(pipeline, source, sink,
		voice.WithDetector(detect.NewEnergyDetector(
			detect.WithDetectorClassifier(classifier),
			detect.WithDetectorThreshold(0.01),
			detect.WithDetectorInterruptThreshold(0.05),
			detect.WithDetectorConfirm(3),
		)),
		voice.WithSilenceDuration(700*time.Millisecond),
		voice.WithFrameSize(100*time.Millisecond),
		voice.WithEventHandler(func(ev voice.Event) {
			if ev.Type == voice.EventTurnInterrupted {
				interrupted = true
			}
			if ev.Type == voice.EventDone {
				cancel()
			}
		}),
	)

	err := session.Run(ctx)
	if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		t.Fatalf("Run: %v", err)
	}

	if !interrupted {
		t.Error("expected barge-in with classifier")
	}
}

// eofAudioSource feeds frames and closes the pipe (EOF), used for TestSession_SourceEOF.
type eofAudioSource struct {
	pipe *audio.Pipe[audio.Frame]
}

func newEOFAudioSource(frames []audio.Frame) *eofAudioSource {
	pipe := audio.NewPipe[audio.Frame](len(frames) + 1)
	go func() {
		for _, f := range frames {
			pipe.Send(f)
		}
		pipe.Close()
	}()
	return &eofAudioSource{pipe: pipe}
}

func (s *eofAudioSource) Stream() audio.Stream[audio.Frame] { return s.pipe }

func TestSession_SourceEOF(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	frames := []audio.Frame{
		makeLoudFrame(samplesPerChunk),
		makeSilentFrame(samplesPerChunk),
	}
	source := newEOFAudioSource(frames)
	sink := &fakeAudioSink{}
	pipeline := newSessionPipeline("hi")

	session := voice.NewSession(pipeline, source, sink)

	err := session.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v (expected nil on source EOF)", err)
	}
}

// multiRoundSource sends Round 1 frames immediately, waits for a signal on
// round2Gate, then sends Round 2 frames.
type multiRoundSource struct {
	pipe *audio.Pipe[audio.Frame]
}

func newMultiRoundSource(ctx context.Context, round2Gate <-chan struct{}) *multiRoundSource {
	pipe := audio.NewPipe[audio.Frame](32)
	go func() {
		const (
			loudChunks   = 5
			silentChunks = 12
		)
		for i := 0; i < loudChunks; i++ {
			if !pipe.Send(makeLoudFrame(samplesPerChunk)) {
				return
			}
		}
		for i := 0; i < silentChunks; i++ {
			if !pipe.Send(makeSilentFrame(samplesPerChunk)) {
				return
			}
		}
		select {
		case <-round2Gate:
		case <-ctx.Done():
			pipe.Interrupt()
			return
		}
		for i := 0; i < loudChunks; i++ {
			if !pipe.Send(makeLoudFrame(samplesPerChunk)) {
				return
			}
		}
		for i := 0; i < silentChunks; i++ {
			if !pipe.Send(makeSilentFrame(samplesPerChunk)) {
				return
			}
		}
		silence := makeSilentFrame(samplesPerChunk)
		for {
			select {
			case <-ctx.Done():
				pipe.Interrupt()
				return
			default:
				if !pipe.Send(silence) {
					return
				}
			}
		}
	}()
	return &multiRoundSource{pipe: pipe}
}

func (s *multiRoundSource) Stream() audio.Stream[audio.Frame] { return s.pipe }

func TestSession_MultiRound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	round2Gate := make(chan struct{})
	source := newMultiRoundSource(ctx, round2Gate)
	sink := &fakeAudioSink{}
	pipeline := newSessionPipeline("ok")

	var rounds int
	var doneCount int
	session := voice.NewSession(pipeline, source, sink,
		voice.WithSilenceDuration(700*time.Millisecond),
		voice.WithFrameSize(100*time.Millisecond),
		voice.WithEventHandler(func(ev voice.Event) {
			if ev.Type == voice.EventTranscriptFinal {
				rounds++
			}
			if ev.Type == voice.EventDone {
				doneCount++
				if doneCount == 1 {
					close(round2Gate)
				}
				if doneCount >= 2 {
					cancel()
				}
			}
		}),
	)

	err := session.Run(ctx)
	if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		t.Fatalf("Run: %v", err)
	}

	if sink.Plays() < 2 {
		t.Errorf("expected at least 2 Play calls for 2 rounds, got %d", sink.Plays())
	}
	if rounds < 2 {
		t.Errorf("expected at least 2 transcript-final events, got %d", rounds)
	}
}

// ---------------------------------------------------------------------------
// Text input (Session.Send) tests
// ---------------------------------------------------------------------------

func TestSession_TextOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sink := &fakeAudioSink{}
	pipeline := newTextOnlyPipeline()

	var gotTranscript, gotDone bool
	session := voice.NewSession(pipeline, nil, sink,
		voice.WithEventHandler(func(ev voice.Event) {
			if ev.Type == voice.EventTranscriptFinal {
				gotTranscript = true
			}
			if ev.Type == voice.EventDone {
				gotDone = true
				cancel()
			}
		}),
	)

	go func() {
		time.Sleep(50 * time.Millisecond)
		session.Send("hello from keyboard")
	}()

	err := session.Run(ctx)
	if err != nil && err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}

	if !gotTranscript {
		t.Error("expected EventTranscriptFinal from text input")
	}
	if !gotDone {
		t.Error("expected EventDone")
	}
	if sink.Plays() < 1 {
		t.Errorf("expected at least 1 Play call, got %d", sink.Plays())
	}
}

func TestSession_TextOnly_MultiRound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sink := &fakeAudioSink{}
	pipeline := newTextOnlyPipeline()

	var doneCount int
	session := voice.NewSession(pipeline, nil, sink,
		voice.WithEventHandler(func(ev voice.Event) {
			if ev.Type == voice.EventDone {
				doneCount++
				if doneCount >= 2 {
					cancel()
				}
			}
		}),
	)

	go func() {
		time.Sleep(50 * time.Millisecond)
		session.Send("message 1")
		time.Sleep(300 * time.Millisecond)
		session.Send("message 2")
	}()

	err := session.Run(ctx)
	if err != nil && err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}

	if doneCount < 2 {
		t.Errorf("expected 2 done events, got %d", doneCount)
	}
	if sink.Plays() < 2 {
		t.Errorf("expected at least 2 Play calls, got %d", sink.Plays())
	}
}

func TestSession_TextInterruptsResponding(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sink := &fakeAudioSink{}
	rt := newDelayedRuntime([]string{"slow reply"}, 2*time.Second)
	pipeline := voice.NewPipeline(
		nil,
		&fakeStreamTTS{fakeTTS: &fakeTTS{}},
		rt,
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	var doneCount int
	session := voice.NewSession(pipeline, nil, sink,
		voice.WithEventHandler(func(ev voice.Event) {
			if ev.Type == voice.EventDone {
				doneCount++
				if doneCount >= 1 {
					cancel()
				}
			}
		}),
	)

	go func() {
		time.Sleep(50 * time.Millisecond)
		session.Send("first message")
		time.Sleep(200 * time.Millisecond)
		session.Send("interrupt!")
	}()

	err := session.Run(ctx)
	if err != nil && err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}
}

func TestSession_TextInterruptStopsOldTurnEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sink := &fakeAudioSink{}
	rt := &staleTokenRuntime{}
	pipeline := voice.NewPipeline(
		nil,
		&fakeStreamTTS{fakeTTS: &fakeTTS{}},
		rt,
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)

	var (
		mu     sync.Mutex
		deltas []string
	)
	session := voice.NewSession(pipeline, nil, sink,
		voice.WithEventHandler(func(ev voice.Event) {
			if ev.Type == voice.EventTextDelta {
				mu.Lock()
				deltas = append(deltas, ev.Text)
				mu.Unlock()
			}
			if ev.Type == voice.EventDone {
				mu.Lock()
				defer mu.Unlock()
				for _, delta := range deltas {
					if delta == "new-turn " {
						cancel()
						return
					}
				}
			}
		}),
	)

	go func() {
		time.Sleep(20 * time.Millisecond)
		session.Send("first")
		time.Sleep(60 * time.Millisecond)
		session.Send("second")
	}()

	err := session.Run(ctx)
	if err != nil && err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var gotOldLate, gotNew bool
	for _, delta := range deltas {
		if delta == "old-late " {
			gotOldLate = true
		}
		if delta == "new-turn " {
			gotNew = true
		}
	}
	if !gotNew {
		t.Fatalf("expected new turn token, got %v", deltas)
	}
	if gotOldLate {
		t.Fatalf("stale token from interrupted turn leaked into session events: %v", deltas)
	}
}

func TestSession_CommitInputEndsHearing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	frames := []audio.Frame{
		makeLoudFrame(samplesPerChunk),
		makeLoudFrame(samplesPerChunk),
		makeLoudFrame(samplesPerChunk),
		makeSilentFrame(samplesPerChunk),
	}
	source := newPacedAudioSource(ctx, frames, 120*time.Millisecond)
	sink := &fakeAudioSink{}
	pipeline := newSessionPipeline("committed")

	var (
		gotTranscript bool
		gotTurnDone   bool
	)
	session := voice.NewSession(pipeline, source, sink,
		voice.WithSilenceDuration(700*time.Millisecond),
		voice.WithFrameSize(100*time.Millisecond),
		voice.WithEventHandler(func(ev voice.Event) {
			if ev.Type == voice.EventTranscriptFinal && ev.Text == "committed" {
				gotTranscript = true
			}
			if ev.Type == voice.EventTurnDone {
				gotTurnDone = true
				cancel()
			}
		}),
	)

	go func() {
		time.Sleep(180 * time.Millisecond)
		session.CommitInput()
	}()

	err := session.Run(ctx)
	if err != nil && err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}
	if !gotTranscript {
		t.Fatal("expected committed transcript after CommitInput")
	}
	if !gotTurnDone {
		t.Fatal("expected EventTurnDone after CommitInput flow")
	}
}

func TestSession_LifecycleEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sink := &fakeAudioSink{}
	pipeline := newTextOnlyPipeline()

	var (
		mu     sync.Mutex
		events []voice.EventType
	)
	session := voice.NewSession(pipeline, nil, sink,
		voice.WithEventHandler(func(ev voice.Event) {
			mu.Lock()
			events = append(events, ev.Type)
			mu.Unlock()
			if ev.Type == voice.EventTurnDone {
				cancel()
			}
		}),
	)

	go func() {
		time.Sleep(50 * time.Millisecond)
		session.Send("hello lifecycle")
	}()

	err := session.Run(ctx)
	if err != nil && err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	indexOf := func(target voice.EventType) int {
		for i, ev := range events {
			if ev == target {
				return i
			}
		}
		return -1
	}

	turnStarted := indexOf(voice.EventTurnStarted)
	responseDone := indexOf(voice.EventResponseDone)
	playStarted := indexOf(voice.EventPlayStarted)
	playDone := indexOf(voice.EventPlayDone)
	turnDone := indexOf(voice.EventTurnDone)
	done := indexOf(voice.EventDone)

	if turnStarted < 0 || responseDone < 0 || playStarted < 0 || playDone < 0 || turnDone < 0 || done < 0 {
		t.Fatalf("missing lifecycle events: %v", events)
	}
	if turnStarted >= responseDone || turnStarted >= playStarted {
		t.Fatalf("unexpected lifecycle order: %v", events)
	}
	if playStarted >= playDone || playDone >= turnDone || turnDone > done {
		t.Fatalf("unexpected lifecycle order: %v", events)
	}
}

func TestSession_MetricsHook(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sink := &fakeAudioSink{}
	pipeline := newTextOnlyPipeline()

	var (
		mu      sync.Mutex
		metrics []speechmetrics.TurnMetrics
	)
	session := voice.NewSession(pipeline, nil, sink,
		voice.WithMetricsHook(speechmetrics.HookFunc(func(m speechmetrics.TurnMetrics) {
			mu.Lock()
			metrics = append(metrics, m)
			mu.Unlock()
		})),
		voice.WithEventHandler(func(ev voice.Event) {
			if ev.Type == voice.EventTurnDone {
				cancel()
			}
		}),
	)

	go func() {
		time.Sleep(50 * time.Millisecond)
		session.Send("metrics please")
	}()

	err := session.Run(ctx)
	if err != nil && err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(metrics) != 1 {
		t.Fatalf("expected 1 metrics record, got %d", len(metrics))
	}
	m := metrics[0]
	if m.SessionID == "" || m.TurnID == "" {
		t.Fatalf("expected session and turn IDs in metrics, got %+v", m)
	}
	if m.EndToEnd <= 0 {
		t.Fatalf("expected end-to-end latency, got %+v", m)
	}
	if m.RunnerFirstToken <= 0 {
		t.Fatalf("expected runner first token latency, got %+v", m)
	}
	if m.TTSFirstAudio <= 0 {
		t.Fatalf("expected tts first audio latency, got %+v", m)
	}
}

func TestSession_MixedAudioAndText(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var frames []audio.Frame
	for i := 0; i < 3; i++ {
		frames = append(frames, makeLoudFrame(samplesPerChunk))
	}
	for i := 0; i < 8; i++ {
		frames = append(frames, makeSilentFrame(samplesPerChunk))
	}
	source := newFakeAudioSource(frames, ctx)
	sink := &fakeAudioSink{}
	pipeline := newSessionPipeline("audio-turn")

	var transcripts []string
	var doneCount int
	var session *voice.Session
	session = voice.NewSession(pipeline, source, sink,
		voice.WithSilenceDuration(700*time.Millisecond),
		voice.WithFrameSize(100*time.Millisecond),
		voice.WithEventHandler(func(ev voice.Event) {
			if ev.Type == voice.EventTranscriptFinal {
				transcripts = append(transcripts, ev.Text)
			}
			if ev.Type == voice.EventDone {
				doneCount++
				if doneCount == 1 {
					go session.Send("text-turn")
				}
				if doneCount >= 2 {
					cancel()
				}
			}
		}),
	)

	err := session.Run(ctx)
	if err != nil && err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}

	if len(transcripts) < 2 {
		t.Errorf("expected at least 2 transcripts (audio+text), got %v", transcripts)
	}
	if sink.Plays() < 2 {
		t.Errorf("expected at least 2 Play calls, got %d", sink.Plays())
	}
}

func TestSession_PlaybackDrainTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	timeoutCh := make(chan voice.Event, 1)
	session := voice.NewSession(
		newTextOnlyPipeline(),
		nil,
		&stickyAudioSink{},
		voice.WithPlaybackDrainTimeout(30*time.Millisecond),
		voice.WithEventHandler(func(ev voice.Event) {
			if ev.Type == voice.EventError && ev.ErrorCode == voice.ErrorCodeTimeout {
				select {
				case timeoutCh <- ev:
				default:
				}
				cancel()
			}
		}),
	)

	go func() {
		time.Sleep(20 * time.Millisecond)
		session.Send("hello")
	}()

	err := session.Run(ctx)
	if err != nil && err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}

	select {
	case ev := <-timeoutCh:
		if ev.TurnID == "" {
			t.Fatalf("expected timeout event to carry turn id, got %+v", ev)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected playback drain timeout event")
	}
}

func TestSession_TurnMetricsIncludeProviderReports(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pipeline := voice.NewPipeline(
		stt.NewFallbackSTT(
			failingStreamSTTAdapter{err: context.DeadlineExceeded},
			okStreamSTTAdapter{text: "metrics"},
		),
		tts.NewFallbackTTS(
			failingStreamTTSAdapter{err: context.DeadlineExceeded},
			okStreamTTSAdapter{payload: "audio"},
		),
		&fakeRuntime{tokens: []string{"reply"}},
		fakeAgent{},
		voice.WithSegmenterOptions(tts.WithMinChars(1)),
	)
	source := newFakeAudioSource([]audio.Frame{
		makeLoudFrame(samplesPerChunk),
		makeLoudFrame(samplesPerChunk),
		makeSilentFrame(samplesPerChunk),
		makeSilentFrame(samplesPerChunk),
		makeSilentFrame(samplesPerChunk),
		makeSilentFrame(samplesPerChunk),
		makeSilentFrame(samplesPerChunk),
		makeSilentFrame(samplesPerChunk),
		makeSilentFrame(samplesPerChunk),
	}, ctx)

	var got speechmetrics.TurnMetrics
	session := voice.NewSession(pipeline, source, &fakeAudioSink{},
		voice.WithSilenceDuration(700*time.Millisecond),
		voice.WithFrameSize(100*time.Millisecond),
		voice.WithMetricsHook(speechmetrics.HookFunc(func(m speechmetrics.TurnMetrics) {
			got = m
		})),
		voice.WithEventHandler(func(ev voice.Event) {
			if ev.Type == voice.EventTurnDone {
				cancel()
			}
		}),
	)

	err := session.Run(ctx)
	if err != nil && err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}

	if got.STTProviderReport.SelectedProvider == "" || len(got.STTProviderReport.Attempts) < 2 {
		t.Fatalf("expected stt provider report in metrics, got %+v", got.STTProviderReport)
	}
	if got.TTSProviderReport.SelectedProvider == "" || len(got.TTSProviderReport.Attempts) < 2 {
		t.Fatalf("expected tts provider report in metrics, got %+v", got.TTSProviderReport)
	}
}
