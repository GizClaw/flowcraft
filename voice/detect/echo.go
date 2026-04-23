package detect

import (
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/voice/audio"
	"github.com/GizClaw/flowcraft/voice/vad"
)

type EchoSuppressorOption func(*EchoSuppressor)

// AcousticEchoCanceller optionally preprocesses microphone capture using a
// playback reference frame before speech detection. Production implementations
// can wrap WebRTC APM or platform-specific AEC; the default NoopAEC preserves
// current behavior.
type AcousticEchoCanceller interface {
	ProcessCapture(capture audio.Frame, reference audio.Frame) audio.Frame
	Reset()
}

type NoopAEC struct{}

func (NoopAEC) ProcessCapture(capture audio.Frame, _ audio.Frame) audio.Frame { return capture }
func (NoopAEC) Reset()                                                        {}

// WithEchoActiveWindow sets how long playback reference is treated as active.
func WithEchoActiveWindow(d time.Duration) EchoSuppressorOption {
	return func(s *EchoSuppressor) { s.activeWindow = d }
}

// WithEchoMinPlaybackRMS sets the minimum playback RMS that enables suppression.
func WithEchoMinPlaybackRMS(v float64) EchoSuppressorOption {
	return func(s *EchoSuppressor) { s.minPlaybackRMS = v }
}

// WithEchoNearendRatio allows near-end speech to pass when mic energy
// significantly exceeds playback energy. Values >1 make suppression looser.
func WithEchoNearendRatio(v float64) EchoSuppressorOption {
	return func(s *EchoSuppressor) { s.nearendRatio = v }
}

func WithEchoCanceller(aec AcousticEchoCanceller) EchoSuppressorOption {
	return func(s *EchoSuppressor) {
		if aec != nil {
			s.aec = aec
		}
	}
}

// EchoSuppressor wraps a SpeechDetector and suppresses likely playback echo
// using recent playback reference energy.
type EchoSuppressor struct {
	base SpeechDetector
	aec  AcousticEchoCanceller

	activeWindow   time.Duration
	minPlaybackRMS float64
	nearendRatio   float64

	mu             sync.RWMutex
	playbackFrame  audio.Frame
	playbackRMS    float64
	lastPlaybackAt time.Time
}

func NewEchoSuppressor(base SpeechDetector, opts ...EchoSuppressorOption) *EchoSuppressor {
	s := &EchoSuppressor{
		base:           base,
		aec:            NoopAEC{},
		activeWindow:   250 * time.Millisecond,
		minPlaybackRMS: 0.01,
		nearendRatio:   1.25,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *EchoSuppressor) FeedPlayback(f audio.Frame) {
	rms := vad.PCM16RMS(f.Data, 2)
	if rms <= 0 {
		return
	}
	s.mu.Lock()
	s.playbackFrame = cloneFrame(f)
	s.playbackRMS = rms
	s.lastPlaybackAt = time.Now()
	s.mu.Unlock()
}

func (s *EchoSuppressor) IsSpeech(f audio.Frame) bool {
	f = s.processCapture(f)
	if s.shouldSuppress(f) {
		return false
	}
	return s.base.IsSpeech(f)
}

func (s *EchoSuppressor) Detect(f audio.Frame) (SpeechAction, []audio.Frame) {
	f = s.processCapture(f)
	if s.shouldSuppress(f) {
		s.base.Reset()
		return SpeechNone, nil
	}
	return s.base.Detect(f)
}

func (s *EchoSuppressor) Reset() {
	if s.aec != nil {
		s.aec.Reset()
	}
	s.base.Reset()
}

func (s *EchoSuppressor) shouldSuppress(f audio.Frame) bool {
	playbackRMS, active := s.currentPlayback()
	if !active || playbackRMS < s.minPlaybackRMS {
		return false
	}
	micRMS := vad.PCM16RMS(f.Data, 2)
	if micRMS <= 0 {
		return false
	}
	return micRMS <= playbackRMS*s.nearendRatio
}

func (s *EchoSuppressor) currentPlayback() (float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lastPlaybackAt.IsZero() {
		return 0, false
	}
	return s.playbackRMS, time.Since(s.lastPlaybackAt) <= s.activeWindow
}

func (s *EchoSuppressor) currentPlaybackFrame() (audio.Frame, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lastPlaybackAt.IsZero() || time.Since(s.lastPlaybackAt) > s.activeWindow {
		return audio.Frame{}, false
	}
	return cloneFrame(s.playbackFrame), true
}

func (s *EchoSuppressor) processCapture(f audio.Frame) audio.Frame {
	if s.aec == nil {
		return f
	}
	ref, ok := s.currentPlaybackFrame()
	if !ok {
		return f
	}
	return s.aec.ProcessCapture(f, ref)
}

func cloneFrame(f audio.Frame) audio.Frame {
	if len(f.Data) == 0 {
		return f
	}
	buf := make([]byte, len(f.Data))
	copy(buf, f.Data)
	f.Data = buf
	return f
}
