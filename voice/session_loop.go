package speech

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/speech/audio"
	"github.com/GizClaw/flowcraft/sdk/speech/detect"
	"github.com/GizClaw/flowcraft/sdk/speech/endpoint"
	speechmetrics "github.com/GizClaw/flowcraft/sdk/speech/metrics"
	"github.com/GizClaw/flowcraft/sdk/speech/provider"
	"github.com/GizClaw/flowcraft/sdk/speech/tts"
	"github.com/rs/xid"
)

type frameResult struct {
	f   audio.Frame
	err error
}

// sessionLoop holds the mutable runtime state for a single Session.Run()
// invocation. It is created at the start of Run and discarded when Run returns.
type sessionLoop struct {
	session  *Session
	ctx      context.Context
	src      audio.Stream[audio.Frame]
	detector detect.SpeechDetector
	decider  endpoint.Decider

	// turn lifecycle
	turnCtx      context.Context
	turnCancel   context.CancelFunc
	turnID       string
	state        SessionState
	audioPipe    *audio.Pipe[audio.Frame]
	uttPipe      *audio.Pipe[tts.Utterance]
	playDone     <-chan struct{}
	playTimeout  <-chan time.Time
	lastPlayDone <-chan struct{}
	pipelineDone <-chan struct{}

	metricsMu   sync.Mutex
	turnMetrics turnMetricsState
	bargeIn     bargeInConfirmState

	partialMu     sync.Mutex
	latestPartial string
}

type turnMetricsState struct {
	startedAt          time.Time
	playStartedAt      time.Time
	runID              string
	sttFirstPartialAt  time.Time
	sttFinalAt         time.Time
	runnerFirstTokenAt time.Time
	ttsFirstAudioAt    time.Time
	sttProviderReport  provider.Report
	ttsProviderReport  provider.Report
	interruptReason    InterruptReason
}

type bargeInConfirmState struct {
	candidateN int
	seed       []audio.Frame
}

func newSessionLoop(s *Session, ctx context.Context) *sessionLoop {
	var src audio.Stream[audio.Frame]
	if s.source != nil {
		src = s.source.Stream()
	}
	return &sessionLoop{
		session:  s,
		ctx:      ctx,
		src:      src,
		detector: s.cfg.detector,
		decider:  s.cfg.decider,
		state:    StateIdle,
	}
}

// ---------------------------------------------------------------------------
// Main loop
// ---------------------------------------------------------------------------

func (l *sessionLoop) run() error {
	stopWarmup := l.warmupTTS()
	defer stopWarmup()
	frameCh := l.startFrameReader()
	l.startPlaybackReferenceReader()

	for {
		select {
		case <-l.ctx.Done():
			l.endTurn()
			l.waitSinkDone()
			return l.ctx.Err()

		case text := <-l.session.textCh:
			l.state = l.onText(text)

		case <-l.session.commitCh:
			l.state = l.onCommit()

		case <-l.session.stopSpeakCh:
			l.state = l.onStopSpeaking()

		case fr, ok := <-frameCh:
			if !ok {
				l.endTurn()
				l.waitSinkDone()
				return nil
			}
			if fr.err != nil {
				l.endTurn()
				l.waitSinkDone()
				if fr.err == io.EOF {
					return nil
				}
				return fr.err
			}
			l.state = l.handleFrame(fr.f)

		case <-l.pipelineDoneCh():
			l.pipelineDone = nil
			if l.state == StateResponding {
				l.state = StatePlayback
			}

		case <-l.playDoneCh():
			l.session.emitEvent(l.currentTurnEvent(Event{Type: EventPlayDone}))
			l.session.emitEvent(l.currentTurnEvent(Event{Type: EventTurnDone}))
			l.session.emitEvent(l.currentTurnEvent(Event{Type: EventDone}))
			l.reportTurnMetrics(false)
			l.playDone = nil
			l.playTimeout = nil
			if l.state == StatePlayback {
				l.state = StateIdle
			}
			l.turnID = ""

		case <-l.playTimeoutCh():
			l.session.emitEvent(l.currentTurnEvent(errorEvent(stageTimeoutError("playback drain"))))
			l.endTurn()
			l.playTimeout = nil
			l.state = StateIdle
			l.turnID = ""
		}
	}
}

// ---------------------------------------------------------------------------
// State handlers
// ---------------------------------------------------------------------------

func (l *sessionLoop) handleFrame(f audio.Frame) SessionState {
	switch l.state {
	case StateIdle:
		return l.onIdle(f)
	case StateHearing:
		return l.onHearing(f)
	case StateResponding, StatePlayback:
		return l.onInterruptable(f)
	}
	return l.state
}

func (l *sessionLoop) onIdle(f audio.Frame) SessionState {
	if l.detector.IsSpeech(f) {
		if l.startPipeline([]audio.Frame{f}) {
			return StateHearing
		}
	}
	return StateIdle
}

func (l *sessionLoop) onHearing(f audio.Frame) SessionState {
	l.audioPipe.Send(f)
	isSpeech := l.detector.IsSpeech(f)
	if l.decider != nil && l.decider.Feed(endpoint.Input{
		IsSpeech:    isSpeech,
		PartialText: l.getLatestPartial(),
	}) == endpoint.Commit {
		l.audioPipe.Close()
		l.audioPipe = nil
		l.detector.Reset()
		l.clearLatestPartial()
		return StateResponding
	}
	return StateHearing
}

func (l *sessionLoop) onInterruptable(f audio.Frame) SessionState {
	action, seed := l.detector.Detect(f)
	switch action {
	case detect.SpeechBargeIn:
		return l.executeBargeIn(seed)
	case detect.SpeechCandidate:
		l.bargeIn.candidateN++
		if len(seed) > 0 {
			l.bargeIn.seed = append([]audio.Frame(nil), seed...)
		}
		if l.bargeIn.candidateN >= l.session.cfg.bargeInConfirm {
			return l.executeBargeIn(l.bargeIn.seed)
		}
	default:
		l.resetBargeInConfirm()
	}
	return l.state
}

func (l *sessionLoop) onText(text string) SessionState {
	if l.state != StateIdle {
		l.setInterruptReason(InterruptReasonTextInterrupt)
		l.emitTurnInterrupted(InterruptReasonTextInterrupt)
		l.endTurn()
		if l.detector != nil {
			l.detector.Reset()
		}
		l.resetBargeInConfirm()
		if l.decider != nil {
			l.decider.Reset()
		}
	}
	if l.startTextPipeline(text) {
		return StateResponding
	}
	return StateIdle
}

func (l *sessionLoop) onCommit() SessionState {
	switch l.state {
	case StateHearing:
		if l.decider != nil {
			_ = l.decider.Feed(endpoint.Input{ExplicitCommit: true})
		}
		if l.audioPipe != nil {
			l.audioPipe.Close()
			l.audioPipe = nil
		}
		if l.detector != nil {
			l.detector.Reset()
		}
		l.resetBargeInConfirm()
		l.clearLatestPartial()
		return StateResponding
	default:
		return l.state
	}
}

func (l *sessionLoop) onStopSpeaking() SessionState {
	switch l.state {
	case StateHearing:
		return l.onCommit()
	case StateResponding, StatePlayback:
		l.setInterruptReason(InterruptReasonManualInterrupt)
		l.emitTurnInterrupted(InterruptReasonManualInterrupt)
		l.endTurn()
		if l.detector != nil {
			l.detector.Reset()
		}
		l.resetBargeInConfirm()
		if l.decider != nil {
			l.decider.Reset()
		}
		return StateIdle
	default:
		return l.state
	}
}

// ---------------------------------------------------------------------------
// Turn lifecycle
// ---------------------------------------------------------------------------

func (l *sessionLoop) startPipeline(seed []audio.Frame) bool {
	l.startTurn()
	l.audioPipe = audio.NewPipe[audio.Frame](16)
	for _, f := range seed {
		l.audioPipe.Send(f)
	}
	if l.decider != nil {
		l.decider.Reset()
	}

	pipeline := l.session.pipelineForTurn()
	if pipeline == nil {
		l.audioPipe.Close()
		l.audioPipe = nil
		l.stopTurn()
		l.session.emitEvent(errorEvent(context.Canceled))
		return false
	}
	ev, err := pipeline.RunAudioStream(l.turnCtx, l.audioPipe)
	if err != nil {
		l.audioPipe.Close()
		l.audioPipe = nil
		l.stopTurn()
		l.session.emitEvent(errorEvent(err))
		return false
	}
	return l.startPlayback(ev)
}

func (l *sessionLoop) startTextPipeline(text string) bool {
	l.startTurn()
	pipeline := l.session.pipelineForTurn()
	if pipeline == nil {
		l.stopTurn()
		l.session.emitEvent(errorEvent(context.Canceled))
		return false
	}
	ev, err := pipeline.RunText(l.turnCtx, text)
	if err != nil {
		l.stopTurn()
		l.session.emitEvent(errorEvent(err))
		return false
	}
	return l.startPlayback(ev)
}

func (l *sessionLoop) startPlayback(ev audio.Stream[Event]) bool {
	l.uttPipe = audio.NewPipe[tts.Utterance](32)
	l.pipelineDone = l.startRelay(ev, l.uttPipe)
	l.playDone = l.session.sink.Play(l.uttPipe)
	if l.session.cfg.playbackDrainTimeout > 0 {
		l.playTimeout = time.After(l.session.cfg.playbackDrainTimeout)
	} else {
		l.playTimeout = nil
	}
	l.lastPlayDone = nil
	l.metricsMu.Lock()
	l.turnMetrics.playStartedAt = time.Now()
	l.metricsMu.Unlock()
	l.session.emitEvent(l.currentTurnEvent(Event{Type: EventPlayStarted}))
	return true
}

func (l *sessionLoop) endTurn() {
	l.reportTurnMetrics(true)
	l.stopTurn()
	l.clearLatestPartial()
	if l.decider != nil {
		l.decider.Reset()
	}
	if l.uttPipe != nil {
		l.uttPipe.Interrupt()
		l.uttPipe = nil
	}
	if l.audioPipe != nil {
		l.audioPipe.Interrupt()
		l.audioPipe = nil
	}
	if l.playDone != nil {
		l.lastPlayDone = l.playDone
	}
	l.playDone = nil
	l.playTimeout = nil
	l.pipelineDone = nil
}

func (l *sessionLoop) waitSinkDone() {
	if l.lastPlayDone != nil {
		if l.session.cfg.playbackDrainTimeout > 0 {
			select {
			case <-l.lastPlayDone:
			case <-time.After(l.session.cfg.playbackDrainTimeout):
				l.session.emitEvent(l.currentTurnEvent(errorEvent(stageTimeoutError("playback drain"))))
			}
		} else {
			<-l.lastPlayDone
		}
		l.lastPlayDone = nil
	}
}

func (l *sessionLoop) startRelay(stream audio.Stream[Event], utt *audio.Pipe[tts.Utterance]) <-chan struct{} {
	done := make(chan struct{})
	turnCtx := l.turnCtx
	turnID := l.turnID
	sessionID := l.session.sessionID
	go func() {
		defer close(done)
		for {
			if turnCtx != nil {
				select {
				case <-turnCtx.Done():
					if utt != nil {
						utt.Interrupt()
					}
					return
				default:
				}
			}
			ev, err := stream.Read()
			if err != nil {
				if utt != nil {
					utt.Close()
				}
				return
			}
			if ev.Type == EventDone {
				continue
			}
			if turnCtx != nil {
				select {
				case <-turnCtx.Done():
					if utt != nil {
						utt.Interrupt()
					}
					return
				default:
				}
			}
			if ev.SessionID == "" {
				ev.SessionID = sessionID
			}
			if ev.TurnID == "" {
				ev.TurnID = turnID
			}
			if ev.Type == EventTranscriptPartial {
				l.setLatestPartial(ev.Text)
			}
			l.observeEventMetrics(ev)
			l.session.emitEvent(ev)
			if ev.Type == EventAudio && utt != nil {
				if !utt.Send(tts.Utterance{Frame: ev.Audio, Text: ev.Text}) {
					return
				}
			}
		}
	}()
	return done
}

func (l *sessionLoop) startTurn() {
	l.stopTurn()
	l.turnCtx, l.turnCancel = context.WithCancel(l.ctx)
	l.turnID = xid.New().String()
	l.metricsMu.Lock()
	l.turnMetrics = turnMetricsState{startedAt: time.Now()}
	l.metricsMu.Unlock()
	l.resetBargeInConfirm()
	l.clearLatestPartial()
	l.session.emitEvent(l.currentTurnEvent(Event{Type: EventTurnStarted}))
}

func (l *sessionLoop) stopTurn() {
	if l.turnCancel != nil {
		l.turnCancel()
		l.turnCancel = nil
	}
	l.turnCtx = nil
	l.resetBargeInConfirm()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (l *sessionLoop) startFrameReader() <-chan frameResult {
	if l.src == nil {
		return nil
	}
	ch := make(chan frameResult, 1)
	go func() {
		defer close(ch)
		for {
			f, err := l.src.Read()
			if err == nil && l.session.cfg.preprocessor != nil {
				f = l.session.cfg.preprocessor.Process(f)
			}
			select {
			case ch <- frameResult{f, err}:
			case <-l.ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

func (l *sessionLoop) startPlaybackReferenceReader() {
	provider, ok := l.session.sink.(PlaybackReferenceProvider)
	if !ok {
		return
	}
	aware, ok := l.detector.(detect.PlaybackAwareDetector)
	if !ok {
		return
	}
	ref := provider.PlaybackReference()
	if ref == nil {
		return
	}
	go func() {
		for {
			f, err := ref.Read()
			if err != nil {
				return
			}
			select {
			case <-l.ctx.Done():
				return
			default:
			}
			aware.FeedPlayback(f)
		}
	}()
}

func (l *sessionLoop) pipelineDoneCh() <-chan struct{} {
	return l.pipelineDone
}

func (l *sessionLoop) playDoneCh() <-chan struct{} {
	return l.playDone
}

func (l *sessionLoop) playTimeoutCh() <-chan time.Time {
	return l.playTimeout
}

func (l *sessionLoop) resetBargeInConfirm() {
	l.bargeIn = bargeInConfirmState{}
}

func (l *sessionLoop) executeBargeIn(seed []audio.Frame) SessionState {
	l.setInterruptReason(InterruptReasonUserBargeIn)
	l.emitTurnInterrupted(InterruptReasonUserBargeIn)
	l.endTurn()
	l.detector.Reset()
	l.resetBargeInConfirm()
	if l.decider != nil {
		l.decider.Reset()
	}
	if len(seed) == 0 {
		return StateIdle
	}
	if l.startPipeline(seed) {
		return StateHearing
	}
	return StateIdle
}

func (l *sessionLoop) currentTurnEvent(ev Event) Event {
	if ev.SessionID == "" {
		ev.SessionID = l.session.sessionID
	}
	if ev.TurnID == "" {
		ev.TurnID = l.turnID
	}
	return ev
}

func (l *sessionLoop) setInterruptReason(reason InterruptReason) {
	l.metricsMu.Lock()
	l.turnMetrics.interruptReason = reason
	l.metricsMu.Unlock()
}

func (l *sessionLoop) emitTurnInterrupted(reason InterruptReason) {
	if l.turnID == "" {
		return
	}
	l.session.emitEvent(l.currentTurnEvent(Event{
		Type:            EventTurnInterrupted,
		InterruptReason: reason,
	}))
}

func (l *sessionLoop) observeEventMetrics(ev Event) {
	l.metricsMu.Lock()
	defer l.metricsMu.Unlock()
	now := time.Now()
	if ev.RunID != "" && l.turnMetrics.runID == "" {
		l.turnMetrics.runID = ev.RunID
	}
	switch ev.Type {
	case EventTranscriptPartial:
		if l.turnMetrics.sttFirstPartialAt.IsZero() {
			l.turnMetrics.sttFirstPartialAt = now
		}
	case EventTranscriptFinal:
		if report, ok := providerReportFromEvent(ev); ok && l.turnMetrics.sttProviderReport.Operation == "" {
			l.turnMetrics.sttProviderReport = report
		}
		if l.turnMetrics.sttFinalAt.IsZero() {
			l.turnMetrics.sttFinalAt = now
		}
	case EventTextDelta:
		if l.turnMetrics.runnerFirstTokenAt.IsZero() {
			l.turnMetrics.runnerFirstTokenAt = now
		}
	case EventAudio:
		if report, ok := providerReportFromEvent(ev); ok && l.turnMetrics.ttsProviderReport.Operation == "" {
			l.turnMetrics.ttsProviderReport = report
		}
		if l.turnMetrics.ttsFirstAudioAt.IsZero() {
			l.turnMetrics.ttsFirstAudioAt = now
		}
	}
}

func (l *sessionLoop) reportTurnMetrics(interrupted bool) {
	hook := l.session.cfg.metricsHook
	if hook == nil || l.turnID == "" {
		return
	}
	l.metricsMu.Lock()
	tm := l.turnMetrics
	l.turnMetrics = turnMetricsState{}
	l.metricsMu.Unlock()
	if tm.startedAt.IsZero() {
		return
	}
	completedAt := time.Now()
	m := speechmetrics.TurnMetrics{
		SessionID:         l.session.sessionID,
		TurnID:            l.turnID,
		RunID:             tm.runID,
		StartedAt:         tm.startedAt,
		CompletedAt:       completedAt,
		EndToEnd:          completedAt.Sub(tm.startedAt),
		STTProviderReport: tm.sttProviderReport,
		TTSProviderReport: tm.ttsProviderReport,
		Interrupted:       interrupted,
		InterruptReason:   string(tm.interruptReason),
	}
	if !tm.sttFirstPartialAt.IsZero() {
		m.STTFirstPartial = tm.sttFirstPartialAt.Sub(tm.startedAt)
	}
	if !tm.sttFinalAt.IsZero() {
		m.STTFinal = tm.sttFinalAt.Sub(tm.startedAt)
	}
	if !tm.runnerFirstTokenAt.IsZero() {
		m.RunnerFirstToken = tm.runnerFirstTokenAt.Sub(tm.startedAt)
	}
	if !tm.ttsFirstAudioAt.IsZero() {
		m.TTSFirstAudio = tm.ttsFirstAudioAt.Sub(tm.startedAt)
	}
	if !tm.playStartedAt.IsZero() {
		m.PlaybackTotal = completedAt.Sub(tm.playStartedAt)
	}
	hook.OnTurnMetrics(m)
}

// ---------------------------------------------------------------------------
// Partial text tracking (fed by relay goroutine, read by onHearing)
// ---------------------------------------------------------------------------

func (l *sessionLoop) setLatestPartial(text string) {
	l.partialMu.Lock()
	l.latestPartial = text
	l.partialMu.Unlock()
}

func (l *sessionLoop) getLatestPartial() string {
	l.partialMu.Lock()
	defer l.partialMu.Unlock()
	return l.latestPartial
}

func (l *sessionLoop) clearLatestPartial() {
	l.partialMu.Lock()
	l.latestPartial = ""
	l.partialMu.Unlock()
}

// ---------------------------------------------------------------------------
// Session-level TTS warmup
// ---------------------------------------------------------------------------

func (l *sessionLoop) warmupTTS() func() {
	if l.session.pipeline == nil {
		return func() {}
	}
	warmupCtx, warmupCancel := context.WithCancel(l.ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = tts.WarmupTTS(warmupCtx, l.session.pipeline.tts)
	}()
	return func() {
		warmupCancel()
		wg.Wait()
	}
}
