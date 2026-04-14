package main

import (
	"context"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/speech"
	"github.com/GizClaw/flowcraft/sdk/speech/detect"
	"github.com/GizClaw/flowcraft/sdk/speech/vad"
	tea "github.com/charmbracelet/bubbletea"
)

// sessionRef wires /reset to the live session and pipeline without exporting session before it exists.
type sessionRef struct {
	mu       sync.Mutex
	pipeline *speech.Pipeline
	session  *speech.Session
}

func (r *sessionRef) setSession(s *speech.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.session = s
}

// reset interrupts in-flight LLM/TTS/playback; TUI history is cleared separately in handleCommand.
func (r *sessionRef) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pipeline != nil {
		r.pipeline.Abort()
	}
	if r.session != nil {
		r.session.StopSpeaking()
	}
}

// runVoiceUI starts the mic session and bubbletea TUI. Cancel ctx after program.Run returns, then wait on sessionDone.
func runVoiceUI(
	ctx context.Context,
	pipeline *speech.Pipeline,
	source *PortAudioSource,
	sink *PortAudioSink,
	voices []voiceInfo,
	voicePtr *string,
) (sessionDone <-chan struct{}) {
	classifier := vad.NewEnergyVAD(
		vad.WithVADSampleRate(sampleRate),
		vad.WithSpectral(true),
	)
	baseDetector := detect.NewEnergyDetector(
		detect.WithDetectorClassifier(classifier),
		detect.WithDetectorConfirm(3),
	)

	ref := &sessionRef{pipeline: pipeline}
	bridge := &tuiBridge{}
	done := make(chan struct{})

	model := newTUIModel(func(text string) {
		ref.mu.Lock()
		s := ref.session
		ref.mu.Unlock()
		if s != nil {
			s.Send(text)
		}
	}, ref.reset, voices, voicePtr)

	session := speech.NewSession(pipeline, source, sink,
		speech.WithDetector(detect.NewEchoSuppressor(baseDetector)),
		speech.WithCapabilities(speech.SessionCapabilities{
			ClientAEC:    speech.ClientAECUnknown,
			DeviceType:   speech.DeviceTypeDesktop,
			PlaybackMode: speech.PlaybackModeSpeaker,
		}),
		speech.WithSilenceDuration(700*time.Millisecond),
		speech.WithFrameSize(100*time.Millisecond),
		speech.WithPlaybackDrainTimeout(10*time.Minute),
		speech.WithMetricsHook(bridge.metricsHook()),
		speech.WithEventHandler(bridge.speechEventHandler()),
	)
	ref.setSession(session)

	go func() {
		_ = session.Run(ctx)
		close(done)
	}()

	bridge.program = tea.NewProgram(model, tea.WithAltScreen())
	_, _ = bridge.program.Run()
	return done
}
