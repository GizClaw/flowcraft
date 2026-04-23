package speech

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/voice/audio"
	"github.com/GizClaw/flowcraft/voice/provider"
	"github.com/GizClaw/flowcraft/voice/stt"
	"github.com/GizClaw/flowcraft/voice/tts"
	"github.com/GizClaw/flowcraft/sdk/workflow"
	"github.com/rs/xid"
)

// EventType identifies a voice pipeline event.
type EventType string

const (
	EventTurnStarted        EventType = "voice.turn.started"
	EventTranscriptRevision EventType = "voice.transcript.revision"
	EventTranscriptPartial  EventType = "voice.transcript.partial"
	EventTranscriptFinal    EventType = "voice.transcript.final"
	EventTextDelta          EventType = "voice.text.delta"
	EventAudio              EventType = "voice.audio"
	EventResponseDone       EventType = "voice.response.done"
	EventAudioDone          EventType = "voice.audio.done"
	EventPlayStarted        EventType = "voice.play.started"
	EventToolCall           EventType = "voice.tool.call"
	EventToolResult         EventType = "voice.tool.result"
	EventTurnInterrupted    EventType = "voice.turn.interrupted"
	EventTurnDone           EventType = "voice.turn.done"
	EventDone               EventType = "voice.done"
	EventPlayDone           EventType = "voice.play.done"
	EventError              EventType = "voice.error"
)

type InterruptReason string

const (
	InterruptReasonUnknown         InterruptReason = ""
	InterruptReasonUserBargeIn     InterruptReason = "user_barge_in"
	InterruptReasonManualInterrupt InterruptReason = "manual_interrupt"
	InterruptReasonTextInterrupt   InterruptReason = "text_interrupt"
)

// Event is emitted by the voice pipeline.
type Event struct {
	Type               EventType
	Text               string
	Audio              audio.Frame
	Lang               string
	Confidence         float64
	Duration           time.Duration
	Words              []stt.WordTiming
	TranscriptRevision int
	Data               map[string]any
	RunID              string
	TurnID             string
	SessionID          string
	ErrorCode          ErrorCode
	InterruptReason    InterruptReason
}

// PipelineOption configures a Pipeline.
type PipelineOption func(*Pipeline)

type PipelineTimeouts struct {
	STTFirstPartial  time.Duration
	STTFinal         time.Duration
	RunnerFirstToken time.Duration
	TTSFirstAudio    time.Duration
}

func WithSTTOptions(opts ...stt.STTOption) PipelineOption {
	return func(p *Pipeline) { p.sttOpts = opts }
}

func WithTTSOptions(opts ...tts.TTSOption) PipelineOption {
	return func(p *Pipeline) { p.ttsOpts = opts }
}

// WithDynamicTTSOptions registers a callback that is invoked at the start of
// each turn to obtain the current TTS options. The returned options are
// appended after any static options set via WithTTSOptions, so they can
// override values like Speed, Pitch, Emotion, etc.
func WithDynamicTTSOptions(fn func() []tts.TTSOption) PipelineOption {
	return func(p *Pipeline) { p.addDynamicTTSOptionsProvider(fn) }
}

func WithSegmenterOptions(opts ...tts.SegmenterOption) PipelineOption {
	return func(p *Pipeline) { p.segOpts = opts }
}

func WithTimeouts(timeouts PipelineTimeouts) PipelineOption {
	return func(p *Pipeline) { p.timeouts = timeouts }
}

// Pipeline orchestrates STT → Runtime.Run → TTS into a stream of
// voice events using a linear pipeline architecture.
type Pipeline struct {
	stt   stt.STT
	tts   tts.TTS
	rt    workflow.Runtime
	agent workflow.Agent

	turnMu     sync.Mutex
	turnCancel context.CancelFunc

	segOpts []tts.SegmenterOption
	sttOpts []stt.STTOption
	ttsOpts []tts.TTSOption

	dynamicTTSOpts []func() []tts.TTSOption
	timeouts       PipelineTimeouts
	history        []model.Message
	contextID      string
	skipWarmup     bool
}

// currentTTSOpts merges static and dynamic TTS options for the current turn.
func (p *Pipeline) currentTTSOpts() []tts.TTSOption {
	if len(p.dynamicTTSOpts) == 0 {
		return p.ttsOpts
	}
	merged := make([]tts.TTSOption, 0, len(p.ttsOpts)+len(p.dynamicTTSOpts)*4)
	merged = append(merged, p.ttsOpts...)
	for _, provider := range p.dynamicTTSOpts {
		if provider == nil {
			continue
		}
		dynamic := provider()
		if len(dynamic) == 0 {
			continue
		}
		merged = append(merged, dynamic...)
	}
	return merged
}

// WithTurnHistory sets message history for the current turn.
// Only effective when the Runtime has no MemoryFactory configured;
// when a MemoryFactory is present, use WithContextID instead.
func WithTurnHistory(msgs []model.Message) PipelineOption {
	return func(p *Pipeline) {
		p.history = append([]model.Message(nil), msgs...)
	}
}

// WithContextID sets the memory context identifier passed to Runtime.Run.
// When the Runtime is configured with a MemoryFactory, this enables
// automatic history load/save across turns.
func WithContextID(id string) PipelineOption {
	return func(p *Pipeline) { p.contextID = id }
}

func (p *Pipeline) clone() *Pipeline {
	if p == nil {
		return nil
	}
	cp := Pipeline{
		stt:        p.stt,
		tts:        p.tts,
		rt:         p.rt,
		agent:      p.agent,
		timeouts:   p.timeouts,
		contextID:  p.contextID,
		skipWarmup: p.skipWarmup,
		segOpts:    append([]tts.SegmenterOption(nil), p.segOpts...),
		sttOpts:    append([]stt.STTOption(nil), p.sttOpts...),
		ttsOpts:    append([]tts.TTSOption(nil), p.ttsOpts...),
	}
	if len(p.dynamicTTSOpts) > 0 {
		cp.dynamicTTSOpts = append([]func() []tts.TTSOption(nil), p.dynamicTTSOpts...)
	}
	if len(p.history) > 0 {
		cp.history = append([]model.Message(nil), p.history...)
	}
	return &cp
}

func (p *Pipeline) addDynamicTTSOptionsProvider(fn func() []tts.TTSOption) {
	if p == nil || fn == nil {
		return
	}
	p.dynamicTTSOpts = append(p.dynamicTTSOpts, fn)
}

// NewPipeline creates a new voice pipeline.
// The stt parameter may be nil if only text input (RunText) is used.
func NewPipeline(s stt.STT, t tts.TTS, rt workflow.Runtime, agent workflow.Agent, opts ...PipelineOption) *Pipeline {
	p := &Pipeline{stt: s, tts: t, rt: rt, agent: agent}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Abort cancels the currently running execution via context cancellation.
func (p *Pipeline) Abort() bool {
	p.turnMu.Lock()
	cancel := p.turnCancel
	p.turnMu.Unlock()
	if cancel != nil {
		cancel()
		return true
	}
	return false
}

// RunAudio processes a complete audio input in one shot.
// Requires a non-nil STT provider.
func (p *Pipeline) RunAudio(ctx context.Context, input audio.Frame) (audio.Stream[Event], error) {
	if p.stt == nil {
		return nil, fmt.Errorf("speech: RunAudio requires an STT provider")
	}
	out := audio.NewPipe[Event](32)
	stopBind := bindPipeToContext(ctx, out)
	go func() {
		defer stopBind()
		p.runOneShot(ctx, input, out)
	}()
	return out, nil
}

// RunAudioStream processes streaming audio input.
// Requires a non-nil STT provider.
func (p *Pipeline) RunAudioStream(ctx context.Context, input audio.Stream[audio.Frame]) (audio.Stream[Event], error) {
	if p.stt == nil {
		return nil, fmt.Errorf("speech: RunAudioStream requires an STT provider")
	}
	out := audio.NewPipe[Event](32)
	stopBind := bindPipeToContext(ctx, out)
	go func() {
		defer stopBind()
		p.runStream(ctx, input, out)
	}()
	return out, nil
}

// RunText processes text input directly, skipping STT.
// Produces the same Event stream as RunAudio/RunAudioStream.
func (p *Pipeline) RunText(ctx context.Context, text string) (audio.Stream[Event], error) {
	out := audio.NewPipe[Event](32)
	stopBind := bindPipeToContext(ctx, out)
	go func() {
		defer stopBind()
		stopWarmup := p.startWarmup(ctx)
		defer stopWarmup()
		if text == "" {
			p.emitPipe(ctx, out, Event{Type: EventTranscriptFinal, Text: text, TranscriptRevision: 1})
			p.emitPipe(ctx, out, Event{Type: EventDone})
			out.Close()
			return
		}
		p.emitPipe(ctx, out, Event{Type: EventTranscriptFinal, Text: text, TranscriptRevision: 1})
		p.emitPipe(ctx, out, Event{Type: EventTranscriptRevision, Text: text, TranscriptRevision: 1})
		p.runLLMAndTTS(ctx, text, nil, out)
	}()
	return out, nil
}

func (p *Pipeline) emitPipe(ctx context.Context, out *audio.Pipe[Event], ev Event) bool {
	select {
	case <-ctx.Done():
		return false
	default:
		return out.Send(ev)
	}
}

func withRunID(ev Event, runID string) Event {
	if ev.RunID == "" {
		ev.RunID = runID
	}
	return ev
}

func errorEvent(err error) Event {
	if err == nil {
		return Event{Type: EventError, ErrorCode: ErrorCodeUnknown}
	}
	return Event{
		Type:      EventError,
		Text:      err.Error(),
		ErrorCode: ClassifyError(err),
	}
}

func stageTimeoutError(stage string) error {
	return fmt.Errorf("speech: %s timeout", stage)
}

func providerReport(recorder *provider.Recorder, operation string) (provider.Report, bool) {
	if recorder == nil {
		return provider.Report{}, false
	}
	return recorder.Last(operation)
}

func newTranscriptEvent(eventType EventType, r stt.STTResult, revision int) Event {
	ev := Event{
		Type:               eventType,
		Text:               r.Text,
		Audio:              r.Audio,
		Lang:               r.Lang,
		Confidence:         r.Confidence,
		Duration:           r.Duration,
		TranscriptRevision: revision,
	}
	if len(r.Words) > 0 {
		ev.Words = append([]stt.WordTiming(nil), r.Words...)
	}
	return ev
}

// ---------------------------------------------------------------------------
// One-shot path
// ---------------------------------------------------------------------------

func (p *Pipeline) runOneShot(ctx context.Context, input audio.Frame, out *audio.Pipe[Event]) {
	stopWarmup := p.startWarmup(ctx)
	defer stopWarmup()

	input = p.resampleFrame(input)
	sttRecorder := provider.NewRecorder()
	sttCtxBase := provider.WithObserver(ctx, sttRecorder)

	sttCtx := sttCtxBase
	var cancel context.CancelFunc
	if p.timeouts.STTFinal > 0 {
		sttCtx, cancel = context.WithTimeout(sttCtxBase, p.timeouts.STTFinal)
		defer cancel()
	}
	r, err := p.stt.Recognize(sttCtx, input, p.sttOpts...)
	if err != nil {
		ev := errorEvent(err)
		if report, ok := providerReport(sttRecorder, "stt.recognize"); ok {
			ev = withProviderReport(ev, report)
		}
		p.emitPipe(ctx, out, ev)
		out.Close()
		return
	}
	transcriptEvent := newTranscriptEvent(EventTranscriptFinal, r, 1)
	if report, ok := providerReport(sttRecorder, "stt.recognize"); ok {
		transcriptEvent = withProviderReport(transcriptEvent, report)
	}
	if !p.emitPipe(ctx, out, transcriptEvent) {
		out.Close()
		return
	}
	if r.Text == "" {
		p.emitPipe(ctx, out, Event{Type: EventDone})
		out.Close()
		return
	}
	revEvent := newTranscriptEvent(EventTranscriptRevision, r, 1)
	if report, ok := providerReport(sttRecorder, "stt.recognize"); ok {
		revEvent = withProviderReport(revEvent, report)
	}
	if !p.emitPipe(ctx, out, revEvent) {
		out.Close()
		return
	}

	p.runLLMAndTTS(ctx, r.Text, nil, out)
}

// ---------------------------------------------------------------------------
// Streaming path
// ---------------------------------------------------------------------------

func (p *Pipeline) runStream(ctx context.Context, input audio.Stream[audio.Frame], out *audio.Pipe[Event]) {
	stopWarmup := p.startWarmup(ctx)
	defer stopWarmup()

	input = p.resampleInput(input)
	sttRecorder := provider.NewRecorder()
	sttCtx := provider.WithObserver(ctx, sttRecorder)

	if ss, ok := p.stt.(stt.StreamSTT); ok {
		sttCtx, sttCancel := context.WithCancel(sttCtx)
		defer sttCancel()
		sttOut, err := ss.RecognizeStream(sttCtx, input, p.sttOpts...)
		if err != nil {
			ev := errorEvent(err)
			if report, ok := providerReport(sttRecorder, "stt.recognize_stream"); ok {
				ev = withProviderReport(ev, report)
			}
			p.emitPipe(ctx, out, ev)
			out.Close()
			return
		}

		var transcript string
		revision := 0
		sttCh := make(chan sttResultItem, 1)
		go func() {
			defer close(sttCh)
			for {
				r, err := sttOut.Read()
				select {
				case sttCh <- sttResultItem{r: r, err: err}:
				case <-sttCtx.Done():
					return
				}
				if err != nil {
					return
				}
			}
		}()

		var firstTimer <-chan time.Time
		if p.timeouts.STTFirstPartial > 0 {
			firstTimer = time.After(p.timeouts.STTFirstPartial)
		}
		var finalTimer <-chan time.Time
		if p.timeouts.STTFinal > 0 {
			finalTimer = time.After(p.timeouts.STTFinal)
		}

		for {
			select {
			case <-ctx.Done():
				out.Close()
				return
			case <-firstTimer:
				ev := errorEvent(stageTimeoutError("stt first partial"))
				if report, ok := providerReport(sttRecorder, "stt.recognize_stream"); ok {
					ev = withProviderReport(ev, report)
				}
				p.emitPipe(ctx, out, ev)
				out.Close()
				return
			case <-finalTimer:
				ev := errorEvent(stageTimeoutError("stt final"))
				if report, ok := providerReport(sttRecorder, "stt.recognize_stream"); ok {
					ev = withProviderReport(ev, report)
				}
				p.emitPipe(ctx, out, ev)
				out.Close()
				return
			case item, ok := <-sttCh:
				if !ok {
					break
				}
				r, err := item.r, item.err
				if err != nil {
					break
				}
				firstTimer = nil
				revision++
				if r.IsFinal {
					ev := newTranscriptEvent(EventTranscriptFinal, r, revision)
					revEvent := newTranscriptEvent(EventTranscriptRevision, r, revision)
					if report, ok := providerReport(sttRecorder, "stt.recognize_stream"); ok {
						ev = withProviderReport(ev, report)
						revEvent = withProviderReport(revEvent, report)
					}
					p.emitPipe(ctx, out, ev)
					p.emitPipe(ctx, out, revEvent)
					transcript = r.Text
					break
				}
				if !p.emitPipe(ctx, out, newTranscriptEvent(EventTranscriptPartial, r, revision)) {
					out.Close()
					return
				}
				if !p.emitPipe(ctx, out, newTranscriptEvent(EventTranscriptRevision, r, revision)) {
					out.Close()
					return
				}
				continue
			}
			break
		}

		if transcript == "" {
			sttCancel()
			p.emitPipe(ctx, out, Event{Type: EventDone})
			out.Close()
			return
		}

		p.runLLMAndTTS(ctx, transcript, &sttResultStream{ch: sttCh}, out)
	} else {
		var allData []byte
		var format audio.Format
		hasFormat := false
		for {
			f, err := input.Read()
			if err != nil {
				break
			}
			allData = append(allData, f.Data...)
			if !hasFormat {
				format = f.Format
				hasFormat = true
			}
		}
		inputFrame := audio.Frame{Data: allData, Format: format}
		sttCtx := provider.WithObserver(ctx, sttRecorder)
		var cancel context.CancelFunc
		if p.timeouts.STTFinal > 0 {
			sttCtx, cancel = context.WithTimeout(sttCtx, p.timeouts.STTFinal)
			defer cancel()
		}
		r, err := p.stt.Recognize(sttCtx, inputFrame, p.sttOpts...)
		if err != nil {
			ev := errorEvent(err)
			if report, ok := providerReport(sttRecorder, "stt.recognize"); ok {
				ev = withProviderReport(ev, report)
			}
			p.emitPipe(ctx, out, ev)
			out.Close()
			return
		}
		ev := newTranscriptEvent(EventTranscriptFinal, r, 1)
		revEvent := newTranscriptEvent(EventTranscriptRevision, r, 1)
		if report, ok := providerReport(sttRecorder, "stt.recognize"); ok {
			ev = withProviderReport(ev, report)
			revEvent = withProviderReport(revEvent, report)
		}
		if !p.emitPipe(ctx, out, ev) {
			out.Close()
			return
		}
		if r.Text == "" {
			p.emitPipe(ctx, out, Event{Type: EventDone})
			out.Close()
			return
		}
		if !p.emitPipe(ctx, out, revEvent) {
			out.Close()
			return
		}
		p.runLLMAndTTS(ctx, r.Text, nil, out)
	}
}

// ---------------------------------------------------------------------------
// LLM + TTS linear pipeline
// ---------------------------------------------------------------------------

// flowRun is a handle to a running AgentFlow execution.
// Produced by startFlow, consumed by runTTS.
type flowRun struct {
	runID     string
	cancel    context.CancelFunc
	events    <-chan Event
	sentences <-chan string
	done      <-chan struct{}
	runErr    func() error
}

func (p *Pipeline) startFlow(ctx context.Context, transcript string) *flowRun {
	var wg sync.WaitGroup
	var overflow atomic.Int64
	runID := xid.New().String()

	seg := tts.NewSegmenter(p.segOpts...)
	events := make(chan Event, 32)
	sentences := make(chan string, 32)
	done := make(chan struct{})

	eventCh := make(chan workflow.StreamEvent, 256)
	runCtx, runCancel := context.WithCancel(ctx)

	p.turnMu.Lock()
	p.turnCancel = runCancel
	p.turnMu.Unlock()

	var runErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(eventCh)

		req := &workflow.Request{
			RunID:     runID,
			ContextID: p.contextID,
			Message:   model.NewTextMessage(model.RoleUser, transcript),
		}
		var runOpts []workflow.RunOption
		runOpts = append(runOpts, workflow.WithStreamCallback(func(ev workflow.StreamEvent) {
			select {
			case eventCh <- ev:
			default:
				overflow.Add(1)
			}
		}))
		if len(p.history) > 0 {
			runOpts = append(runOpts, workflow.WithHistory(p.history))
		}

		_, err := p.rt.Run(runCtx, p.agent, req, runOpts...)
		if err != nil && runCtx.Err() == nil {
			runErr = err
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(events)
		defer close(sentences)

		for {
			select {
			case <-ctx.Done():
				runCancel()
				return
			case ev, ok := <-eventCh:
				if !ok {
					if runErr != nil {
						select {
						case events <- withRunID(errorEvent(runErr), runID):
						case <-ctx.Done():
						}
					}
					if n := overflow.Load(); n > 0 {
						select {
						case events <- withRunID(Event{
							Type:      EventError,
							ErrorCode: ErrorCodeInternal,
							Text:      fmt.Sprintf("speech: %d stream events dropped due to backpressure", n),
						}, runID):
						case <-ctx.Done():
						}
					}
					if last := seg.Flush(); last != "" {
						select {
						case sentences <- last:
						case <-ctx.Done():
						}
					}
					return
				}

				payload, ok := ev.Payload.(map[string]any)
				if !ok {
					continue
				}

				switch ev.Type {
				case "token":
					content, _ := payload["content"].(string)
					if content == "" {
						continue
					}
					select {
					case events <- withRunID(Event{Type: EventTextDelta, Text: content}, runID):
					case <-ctx.Done():
						runCancel()
						return
					}
					if sentence, segOK := seg.Feed(content); segOK && sentence != "" {
						select {
						case sentences <- sentence:
						case <-ctx.Done():
							runCancel()
							return
						}
					}
				case "tool_call":
					name, _ := payload["name"].(string)
					select {
					case events <- withRunID(Event{Type: EventToolCall, Text: name, Data: payload}, runID):
					case <-ctx.Done():
						runCancel()
						return
					}
				case "tool_result":
					content, _ := payload["content"].(string)
					select {
					case events <- withRunID(Event{Type: EventToolResult, Text: content, Data: payload}, runID):
					case <-ctx.Done():
						runCancel()
						return
					}
				}
			}
		}
	}()

	go func() {
		wg.Wait()
		close(done)
	}()

	return &flowRun{
		runID:     runID,
		cancel:    runCancel,
		events:    events,
		sentences: sentences,
		done:      done,
		runErr:    func() error { return runErr },
	}
}

func (p *Pipeline) drainSttTail(tail audio.Stream[stt.STTResult], runID string, out *audio.Pipe[Event]) {
	revision := 0
	for {
		r, err := tail.Read()
		if err != nil {
			return
		}
		revision++
		if r.IsFinal {
			out.Send(withRunID(newTranscriptEvent(EventTranscriptFinal, r, revision), runID))
			out.Send(withRunID(newTranscriptEvent(EventTranscriptRevision, r, revision), runID))
		} else {
			out.Send(withRunID(newTranscriptEvent(EventTranscriptPartial, r, revision), runID))
			out.Send(withRunID(newTranscriptEvent(EventTranscriptRevision, r, revision), runID))
		}
	}
}

func (p *Pipeline) runTTS(ctx context.Context, flow *flowRun, out *audio.Pipe[Event], awaitExtra ...func()) {
	sentencePipe := audio.NewPipe[string](32)
	defer bindPipeToContext(ctx, sentencePipe)()

	responseDone := make(chan struct{}, 1)
	sentenceStarted := make(chan struct{}, 1)

	var fwdWg sync.WaitGroup
	eventFwdDone := make(chan struct{})

	fwdWg.Add(1)
	go func() {
		defer fwdWg.Done()
		defer close(eventFwdDone)
		defer func() {
			for range flow.events {
			}
			p.emitPipe(ctx, out, withRunID(Event{Type: EventResponseDone}, flow.runID))
			select {
			case responseDone <- struct{}{}:
			default:
			}
		}()

		var firstTokenTimer <-chan time.Time
		if p.timeouts.RunnerFirstToken > 0 {
			firstTokenTimer = time.After(p.timeouts.RunnerFirstToken)
		}

		for {
			select {
			case <-ctx.Done():
				flow.cancel()
				return
			case <-firstTokenTimer:
				flow.cancel()
				out.Send(withRunID(errorEvent(stageTimeoutError("runner first token")), flow.runID))
				return
			case ev, ok := <-flow.events:
				if !ok {
					return
				}
				if ev.Type == EventTextDelta {
					firstTokenTimer = nil
				}
				out.Send(ev)
			}
		}
	}()

	fwdWg.Add(1)
	go func() {
		defer fwdWg.Done()
		defer sentencePipe.Close()
		for s := range flow.sentences {
			select {
			case sentenceStarted <- struct{}{}:
			default:
			}
			sentencePipe.Send(s)
		}
		<-eventFwdDone
	}()

	awaitAll := func() {
		<-flow.done
		fwdWg.Wait()
		for _, f := range awaitExtra {
			f()
		}
	}

	ttsOpts := p.currentTTSOpts()
	ttsRecorder := provider.NewRecorder()
	ttsCtx := provider.WithObserver(ctx, ttsRecorder)

	var ttsStream audio.Stream[tts.Utterance]
	if st, ok := p.tts.(tts.StreamTTS); ok {
		var ttsErr error
		ttsStream, ttsErr = st.SynthesizeStream(ttsCtx, sentencePipe, ttsOpts...)
		if ttsErr != nil {
			ev := withRunID(errorEvent(ttsErr), flow.runID)
			if report, ok := providerReport(ttsRecorder, "tts.synthesize_stream"); ok {
				ev = withProviderReport(ev, report)
			}
			p.emitPipe(ctx, out, ev)
			drainStream[string](sentencePipe)
			p.emitPipe(ctx, out, Event{Type: EventDone})
			awaitAll()
			out.Close()
			return
		}
	} else {
		ttsPipe := audio.NewPipe[tts.Utterance](16)
		ttsStream = ttsPipe
		go func() {
			defer ttsPipe.Close()
			seq := 0
			for {
				sentence, err := sentencePipe.Read()
				if err != nil {
					return
				}
				rc, synthErr := p.tts.Synthesize(ttsCtx, sentence, ttsOpts...)
				if synthErr != nil {
					ev := withRunID(errorEvent(synthErr), flow.runID)
					if report, ok := providerReport(ttsRecorder, "tts.synthesize"); ok {
						ev = withProviderReport(ev, report)
					}
					p.emitPipe(ctx, out, ev)
					continue
				}
				data, readErr := io.ReadAll(rc)
				_ = rc.Close()
				if readErr != nil || len(data) == 0 {
					continue
				}
				if !ttsPipe.Send(tts.Utterance{
					Frame:    audio.Frame{Data: data},
					Text:     sentence,
					Sequence: seq,
				}) {
					return
				}
				seq++
			}
		}()
	}

	if pipe, ok := ttsStream.(*audio.Pipe[tts.Utterance]); ok {
		defer bindPipeToContext(ctx, pipe)()
	}

	type uttItem struct {
		utt tts.Utterance
		err error
	}
	uttCh := make(chan uttItem, 1)
	go func() {
		for {
			utt, err := ttsStream.Read()
			select {
			case uttCh <- uttItem{utt: utt, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var firstAudioTimer <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			goto ttsDone
		case <-sentenceStarted:
			if firstAudioTimer == nil && p.timeouts.TTSFirstAudio > 0 {
				firstAudioTimer = time.After(p.timeouts.TTSFirstAudio)
			}
		case <-firstAudioTimer:
			ev := withRunID(errorEvent(stageTimeoutError("tts first audio")), flow.runID)
			if report, ok := providerReport(ttsRecorder, "tts.synthesize_stream"); ok {
				ev = withProviderReport(ev, report)
			} else if report, ok := providerReport(ttsRecorder, "tts.synthesize"); ok {
				ev = withProviderReport(ev, report)
			}
			p.emitPipe(ctx, out, ev)
			goto ttsDone
		case item := <-uttCh:
			utt, err := item.utt, item.err
			if err != nil {
				goto ttsDone
			}
			firstAudioTimer = nil
			ev := withRunID(Event{
				Type:  EventAudio,
				Text:  utt.Text,
				Audio: utt.Frame,
				Data:  map[string]any{"chunk_id": utt.ChunkID},
			}, flow.runID)
			if report, ok := providerReport(ttsRecorder, "tts.synthesize_stream"); ok {
				ev = withProviderReport(ev, report)
			} else if report, ok := providerReport(ttsRecorder, "tts.synthesize"); ok {
				ev = withProviderReport(ev, report)
			}
			p.emitPipe(ctx, out, ev)
		}
	}
ttsDone:

	select {
	case <-responseDone:
	default:
		p.emitPipe(ctx, out, withRunID(Event{Type: EventResponseDone}, flow.runID))
	}
	p.emitPipe(ctx, out, withRunID(Event{Type: EventAudioDone}, flow.runID))
	p.emitPipe(ctx, out, withRunID(Event{Type: EventDone}, flow.runID))

	awaitAll()
	out.Close()
}

func (p *Pipeline) runLLMAndTTS(
	ctx context.Context,
	transcript string,
	sttTail audio.Stream[stt.STTResult],
	out *audio.Pipe[Event],
) {
	flow := p.startFlow(ctx, transcript)
	var sttWg sync.WaitGroup
	if sttTail != nil {
		sttWg.Add(1)
		go func() {
			defer sttWg.Done()
			p.drainSttTail(sttTail, flow.runID, out)
		}()
	}
	p.runTTS(ctx, flow, out, sttWg.Wait)
}

type sttResultItem struct {
	r   stt.STTResult
	err error
}

type sttResultStream struct {
	ch <-chan sttResultItem
}

func (s *sttResultStream) Read() (stt.STTResult, error) {
	item, ok := <-s.ch
	if !ok {
		return stt.STTResult{}, io.EOF
	}
	return item.r, item.err
}

func drainStream[T any](s audio.Stream[T]) {
	if s == nil {
		return
	}
	for {
		_, err := s.Read()
		if err != nil {
			return
		}
	}
}

func bindPipeToContext[T any](ctx context.Context, pipe *audio.Pipe[T]) func() bool {
	if ctx == nil || pipe == nil {
		return func() bool { return false }
	}
	return context.AfterFunc(ctx, pipe.Interrupt)
}

func (p *Pipeline) startWarmup(ctx context.Context) func() {
	if p.skipWarmup {
		return func() {}
	}
	warmupCtx, warmupCancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = tts.WarmupTTS(warmupCtx, p.tts)
	}()
	return func() {
		warmupCancel()
		wg.Wait()
	}
}

func (p *Pipeline) resampleInput(input audio.Stream[audio.Frame]) audio.Stream[audio.Frame] {
	rate := stt.ApplySTTOptions(p.sttOpts...).TargetSampleRate
	if rate <= 0 {
		return input
	}
	return audio.ResampleStream(input, rate)
}

func (p *Pipeline) resampleFrame(f audio.Frame) audio.Frame {
	rate := stt.ApplySTTOptions(p.sttOpts...).TargetSampleRate
	if rate <= 0 || f.Format.Codec != audio.CodecPCM || f.Format.SampleRate == rate || f.Format.SampleRate <= 0 {
		return f
	}
	channels := f.Format.Channels
	if channels <= 0 {
		channels = 1
	}
	f.Data = audio.ResamplePCM16(f.Data, f.Format.SampleRate, rate, channels)
	f.Format.SampleRate = rate
	return f
}
