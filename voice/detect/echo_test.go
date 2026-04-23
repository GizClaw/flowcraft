package detect

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/voice/audio"
)

type recordingAEC struct {
	calls int
}

func (a *recordingAEC) ProcessCapture(capture audio.Frame, reference audio.Frame) audio.Frame {
	a.calls++
	return capture
}

func (a *recordingAEC) Reset() {}

func makePCMFrame(samples int, amp int16) audio.Frame {
	b := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(amp))
	}
	return audio.Frame{
		Data:   b,
		Format: audio.Format{Codec: audio.CodecPCM, SampleRate: 16000, Channels: 1, BitDepth: 16},
	}
}

func TestEchoSuppressor_SuppressesPlaybackLikeInput(t *testing.T) {
	base := NewEnergyDetector(WithDetectorThreshold(0.01), WithDetectorInterruptThreshold(0.02))
	s := NewEchoSuppressor(base)

	playback := makePCMFrame(1600, 12000)
	micEcho := makePCMFrame(1600, 12000)

	s.FeedPlayback(playback)
	if s.IsSpeech(micEcho) {
		t.Fatal("expected playback-like mic frame to be suppressed")
	}
}

func TestEchoSuppressor_AllowsStrongerNearendSpeech(t *testing.T) {
	base := NewEnergyDetector(WithDetectorThreshold(0.01), WithDetectorInterruptThreshold(0.02))
	s := NewEchoSuppressor(base, WithEchoNearendRatio(1.1))

	playback := makePCMFrame(1600, 8000)
	nearend := makePCMFrame(1600, 15000)

	s.FeedPlayback(playback)
	if !s.IsSpeech(nearend) {
		t.Fatal("expected stronger near-end speech to pass through suppressor")
	}
}

func TestEchoSuppressor_UsesConfiguredAEC(t *testing.T) {
	base := NewEnergyDetector(WithDetectorThreshold(0.01), WithDetectorInterruptThreshold(0.02))
	aec := &recordingAEC{}
	s := NewEchoSuppressor(base, WithEchoCanceller(aec))

	s.FeedPlayback(makePCMFrame(1600, 8000))
	_ = s.IsSpeech(makePCMFrame(1600, 12000))

	if aec.calls == 0 {
		t.Fatal("expected configured AEC to process capture frame")
	}
}

func TestNoopAEC_Reset(t *testing.T) {
	aec := NoopAEC{}
	aec.Reset()
}

func TestEchoSuppressor_Detect_Suppressed(t *testing.T) {
	base := NewEnergyDetector(WithDetectorThreshold(0.01), WithDetectorInterruptThreshold(0.02))
	s := NewEchoSuppressor(base)

	s.FeedPlayback(makePCMFrame(1600, 12000))

	action, frames := s.Detect(makePCMFrame(1600, 12000))
	if action != SpeechNone {
		t.Fatalf("expected SpeechNone when echo-suppressed, got %v", action)
	}
	if frames != nil {
		t.Fatal("expected nil frames when suppressed")
	}
}

func TestEchoSuppressor_Detect_NotSuppressed(t *testing.T) {
	base := NewEnergyDetector(
		WithDetectorThreshold(0.01),
		WithDetectorInterruptThreshold(0.01),
		WithDetectorConfirm(1),
	)
	s := NewEchoSuppressor(base, WithEchoNearendRatio(1.0))

	loud := makePCMFrame(1600, 20000)
	action, _ := s.Detect(loud)
	if action != SpeechCandidate {
		t.Fatalf("expected SpeechCandidate without playback, got %v", action)
	}
}

func TestEchoSuppressor_Reset(t *testing.T) {
	base := NewEnergyDetector(WithDetectorThreshold(0.01))
	aec := &recordingAEC{}
	s := NewEchoSuppressor(base, WithEchoCanceller(aec))
	s.Reset()
}

func TestWithEchoActiveWindow(t *testing.T) {
	base := NewEnergyDetector(WithDetectorThreshold(0.01))
	s := NewEchoSuppressor(base, WithEchoActiveWindow(100*time.Millisecond))

	s.FeedPlayback(makePCMFrame(1600, 12000))
	time.Sleep(150 * time.Millisecond)

	if !s.IsSpeech(makePCMFrame(1600, 12000)) {
		t.Fatal("expected speech after active window expired")
	}
}

func TestWithEchoMinPlaybackRMS(t *testing.T) {
	base := NewEnergyDetector(WithDetectorThreshold(0.005))
	s := NewEchoSuppressor(base, WithEchoMinPlaybackRMS(0.99))

	s.FeedPlayback(makePCMFrame(1600, 500))

	if !s.IsSpeech(makePCMFrame(1600, 500)) {
		t.Fatal("expected speech when playback RMS is below minPlaybackRMS threshold")
	}
}

func TestEchoSuppressor_FeedPlayback_ZeroRMS(t *testing.T) {
	base := NewEnergyDetector(WithDetectorThreshold(0.01))
	s := NewEchoSuppressor(base)

	s.FeedPlayback(makePCMFrame(1600, 0))
	if !s.IsSpeech(makePCMFrame(1600, 10000)) {
		t.Fatal("expected speech — zero-RMS playback should not enable suppression")
	}
}

func TestEchoSuppressor_ShouldSuppress_ZeroMicRMS(t *testing.T) {
	base := NewEnergyDetector(WithDetectorThreshold(0.01))
	s := NewEchoSuppressor(base)

	s.FeedPlayback(makePCMFrame(1600, 10000))

	silent := makePCMFrame(1600, 0)
	if s.IsSpeech(silent) {
		t.Fatal("expected silence to not be speech")
	}
}

func TestCloneFrame_EmptyData(t *testing.T) {
	f := audio.Frame{Data: nil}
	c := cloneFrame(f)
	if c.Data != nil {
		t.Fatal("expected nil data for empty clone")
	}
}

func TestWithEchoCanceller_Nil(t *testing.T) {
	base := NewEnergyDetector()
	s := NewEchoSuppressor(base, WithEchoCanceller(nil))
	if s.aec == nil {
		t.Fatal("expected aec to remain non-nil when WithEchoCanceller(nil) is passed")
	}
}
