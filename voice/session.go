package speech

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/voice/detect"
	"github.com/GizClaw/flowcraft/voice/endpoint"
	speechmetrics "github.com/GizClaw/flowcraft/voice/metrics"
	"github.com/GizClaw/flowcraft/voice/preprocess"
	"github.com/GizClaw/flowcraft/voice/tts"
	"github.com/rs/xid"
)

// SessionState represents the current state of the voice session.
type SessionState int

const (
	StateIdle SessionState = iota
	StateHearing
	StateResponding
	StatePlayback
)

// SessionOption configures a Session.
type SessionOption func(*sessionConfig)

type ClientAECMode string

const (
	ClientAECUnknown  ClientAECMode = "unknown"
	ClientAECHardware ClientAECMode = "hardware"
	ClientAECOS       ClientAECMode = "os"
	ClientAECBrowser  ClientAECMode = "browser"
	ClientAECDisabled ClientAECMode = "disabled"
)

type DeviceType string

const (
	DeviceTypeUnknown DeviceType = "unknown"
	DeviceTypeDesktop DeviceType = "desktop"
	DeviceTypeBrowser DeviceType = "browser"
	DeviceTypeMobile  DeviceType = "mobile"
)

type PlaybackMode string

const (
	PlaybackModeUnknown PlaybackMode = "unknown"
	PlaybackModeSpeaker PlaybackMode = "speaker"
	PlaybackModeHeadset PlaybackMode = "headset"
)

// SessionCapabilities describes client-side media processing and device hints.
type SessionCapabilities struct {
	ClientAEC    ClientAECMode
	ClientNS     bool
	ClientAGC    bool
	DeviceType   DeviceType
	PlaybackMode PlaybackMode
}

type sessionConfig struct {
	silenceDuration      time.Duration
	frameSize            time.Duration
	playbackDrainTimeout time.Duration
	bargeInConfirm       int
	onEvent              func(Event)
	detector             detect.SpeechDetector
	decider              endpoint.Decider
	preprocessor         preprocess.Processor
	metricsHook          speechmetrics.Hook
	capabilities         SessionCapabilities
	voiceProfile         VoiceProfile
	voiceProfileSet      bool
}

// WithDetector sets a custom SpeechDetector.
// If not set, a default EnergyDetector is used.
func WithDetector(d detect.SpeechDetector) SessionOption {
	return func(c *sessionConfig) { c.detector = d }
}

func WithSilenceDuration(d time.Duration) SessionOption {
	return func(c *sessionConfig) { c.silenceDuration = d }
}

func WithEventHandler(fn func(Event)) SessionOption {
	return func(c *sessionConfig) { c.onEvent = fn }
}

func WithFrameSize(d time.Duration) SessionOption {
	return func(c *sessionConfig) { c.frameSize = d }
}

func WithPlaybackDrainTimeout(d time.Duration) SessionOption {
	return func(c *sessionConfig) { c.playbackDrainTimeout = d }
}

func WithBargeInConfirm(n int) SessionOption {
	return func(c *sessionConfig) { c.bargeInConfirm = n }
}

func WithCapabilities(capabilities SessionCapabilities) SessionOption {
	return func(c *sessionConfig) { c.capabilities = capabilities }
}

func WithEndpointDecider(decider endpoint.Decider) SessionOption {
	return func(c *sessionConfig) { c.decider = decider }
}

func WithMetricsHook(hook speechmetrics.Hook) SessionOption {
	return func(c *sessionConfig) { c.metricsHook = hook }
}

func WithPreprocessor(processor preprocess.Processor) SessionOption {
	return func(c *sessionConfig) { c.preprocessor = processor }
}

func WithPreprocessors(processors ...preprocess.Processor) SessionOption {
	return func(c *sessionConfig) { c.preprocessor = preprocess.NewChain(processors...) }
}

func WithVoiceProfile(profile VoiceProfile) SessionOption {
	return func(c *sessionConfig) {
		c.voiceProfile = profile
		c.voiceProfileSet = true
	}
}

// Session implements a voice conversation state machine.
//
//	Audio:  IDLE → HEARING → RESPONDING → PLAYBACK → IDLE
//	Text:   IDLE → RESPONDING → PLAYBACK → IDLE
//
// Both audio barge-in and text input (Send) can interrupt any state.
type Session struct {
	pipeline    *Pipeline
	source      AudioSource // nil for text-only sessions
	sink        AudioSink
	cfg         sessionConfig
	sessionID   string
	textCh      chan string
	commitCh    chan struct{}
	stopSpeakCh chan struct{}
}

// NewSession creates a new voice session.
// The src parameter may be nil for text-only sessions (use Send to inject text).
func NewSession(p *Pipeline, src AudioSource, sink AudioSink, opts ...SessionOption) *Session {
	cfg := sessionConfig{
		silenceDuration: 700 * time.Millisecond,
		frameSize:       100 * time.Millisecond,
		bargeInConfirm:  2,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.detector == nil && src != nil {
		cfg.detector = detect.NewEnergyDetector()
	}
	if cfg.decider == nil && src != nil {
		cfg.decider = endpoint.NewSilenceDecider(cfg.silenceDuration, cfg.frameSize)
	}
	if cfg.bargeInConfirm <= 0 {
		cfg.bargeInConfirm = 1
	}
	return &Session{
		pipeline:    p,
		source:      src,
		sink:        sink,
		cfg:         cfg,
		sessionID:   xid.New().String(),
		textCh:      make(chan string, 8),
		commitCh:    make(chan struct{}, 1),
		stopSpeakCh: make(chan struct{}, 1),
	}
}

// Run starts the session loop. It blocks until ctx is cancelled or the
// audio source is exhausted. For text-only sessions (source == nil), Run
// blocks until ctx is cancelled.
func (s *Session) Run(ctx context.Context) error {
	return newSessionLoop(s, ctx).run()
}

// Send injects a text message into the session, skipping STT.
// If the session is currently responding or playing, it triggers a barge-in.
// Returns false if the internal buffer is full (message dropped).
// Safe to call from any goroutine.
func (s *Session) Send(text string) bool {
	select {
	case s.textCh <- text:
		return true
	default:
		return false
	}
}

// CommitInput explicitly commits the current audio input turn.
// It is a no-op when the session is not currently hearing audio.
func (s *Session) CommitInput() bool {
	select {
	case s.commitCh <- struct{}{}:
		return true
	default:
		return false
	}
}

// StopSpeaking signals that the current speaking phase should stop.
// During hearing it behaves like CommitInput; during responding/playback it
// acts as a turn interruption hint.
func (s *Session) StopSpeaking() bool {
	select {
	case s.stopSpeakCh <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Session) SessionID() string { return s.sessionID }

func (s *Session) Capabilities() SessionCapabilities { return s.cfg.capabilities }

func (s *Session) VoiceProfile() (VoiceProfile, bool) {
	return s.cfg.voiceProfile, s.cfg.voiceProfileSet
}

func (s *Session) pipelineForTurn() *Pipeline {
	if s.pipeline == nil {
		return nil
	}
	p := s.pipeline.clone()
	p.skipWarmup = true
	if p.contextID == "" {
		p.contextID = s.sessionID
	}
	if s.cfg.voiceProfileSet {
		profile := s.cfg.voiceProfile
		p.addDynamicTTSOptionsProvider(func() []tts.TTSOption {
			return profile.TTSOptions()
		})
	}
	return p
}

func (s *Session) emitEvent(ev Event) {
	if ev.SessionID == "" {
		ev.SessionID = s.sessionID
	}
	if s.cfg.onEvent != nil {
		s.cfg.onEvent(ev)
	}
}
