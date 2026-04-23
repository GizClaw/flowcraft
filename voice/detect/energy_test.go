package detect

import (
	"encoding/binary"
	"testing"

	"github.com/GizClaw/flowcraft/voice/audio"
)

type fakeClassifier struct {
	score    float64
	isSpeech bool
}

func (f *fakeClassifier) Classify([]byte) (float64, bool) {
	return f.score, f.isSpeech
}

func TestEnergyDetector_IsSpeech_RMS(t *testing.T) {
	d := NewEnergyDetector(WithDetectorThreshold(0.01))

	silent := makePCMFrame(160, 0)
	if d.IsSpeech(silent) {
		t.Fatal("expected silence to not be speech")
	}

	loud := makePCMFrame(160, 10000)
	if !d.IsSpeech(loud) {
		t.Fatal("expected loud frame to be speech")
	}
}

func TestEnergyDetector_IsSpeech_Classifier(t *testing.T) {
	cls := &fakeClassifier{score: 0.6, isSpeech: true}
	d := NewEnergyDetector(WithDetectorClassifier(cls))

	f := makePCMFrame(160, 5000)
	if !d.IsSpeech(f) {
		t.Fatal("expected classifier-detected speech")
	}

	cls.score = 0.1
	if d.IsSpeech(f) {
		t.Fatal("expected low classifier score to not be speech")
	}
}

func TestEnergyDetector_Detect_BargeIn(t *testing.T) {
	d := NewEnergyDetector(
		WithDetectorInterruptThreshold(0.01),
		WithDetectorConfirm(2),
		WithDetectorBoost(1.0),
	)

	loud := makePCMFrame(160, 10000)

	action, _ := d.Detect(loud)
	if action != SpeechNone {
		t.Fatal("expected SpeechNone on first loud frame")
	}

	action, seed := d.Detect(loud)
	if action != SpeechCandidate {
		t.Fatalf("expected SpeechCandidate after confirmN frames, got %v", action)
	}
	if len(seed) != 2 {
		t.Fatalf("expected 2 seed frames, got %d", len(seed))
	}
}

func TestEnergyDetector_Detect_ResetOnQuiet(t *testing.T) {
	d := NewEnergyDetector(
		WithDetectorInterruptThreshold(0.01),
		WithDetectorConfirm(3),
	)

	loud := makePCMFrame(160, 10000)
	silent := makePCMFrame(160, 0)

	d.Detect(loud)
	d.Detect(loud)

	action, _ := d.Detect(silent)
	if action != SpeechNone {
		t.Fatal("expected SpeechNone after quiet frame resets counter")
	}

	action, _ = d.Detect(loud)
	if action != SpeechNone {
		t.Fatal("expected SpeechNone — counter was reset")
	}
}

func TestEnergyDetector_Reset(t *testing.T) {
	d := NewEnergyDetector(
		WithDetectorInterruptThreshold(0.01),
		WithDetectorConfirm(3),
	)

	loud := makePCMFrame(160, 10000)
	d.Detect(loud)
	d.Detect(loud)

	d.Reset()

	action, _ := d.Detect(loud)
	if action != SpeechNone {
		t.Fatal("expected SpeechNone after Reset")
	}
}

func TestWithDetectorBoost(t *testing.T) {
	d := NewEnergyDetector(
		WithDetectorInterruptThreshold(0.01),
		WithDetectorBoost(100.0),
		WithDetectorConfirm(1),
	)

	loud := makePCMFrame(160, 5000)
	action, _ := d.Detect(loud)
	if action != SpeechNone {
		t.Fatal("expected SpeechNone with very high boost")
	}
}

func TestWithDetectorClassifier_DefaultThresholds(t *testing.T) {
	cls := &fakeClassifier{score: 0.5, isSpeech: true}
	d := NewEnergyDetector(WithDetectorClassifier(cls))

	if d.speechThreshold != 0.45 {
		t.Fatalf("expected default classifier speech threshold 0.45, got %v", d.speechThreshold)
	}
	if d.interruptThreshold != 0.55 {
		t.Fatalf("expected default classifier interrupt threshold 0.55, got %v", d.interruptThreshold)
	}
}

func TestWithDetectorClassifier_CustomThresholdsPreserved(t *testing.T) {
	cls := &fakeClassifier{score: 0.5, isSpeech: true}
	d := NewEnergyDetector(
		WithDetectorThreshold(0.3),
		WithDetectorInterruptThreshold(0.4),
		WithDetectorClassifier(cls),
	)

	if d.speechThreshold != 0.3 {
		t.Fatalf("expected custom speech threshold 0.3, got %v", d.speechThreshold)
	}
	if d.interruptThreshold != 0.4 {
		t.Fatalf("expected custom interrupt threshold 0.4, got %v", d.interruptThreshold)
	}
}

func TestEnergyDetector_Detect_WithClassifier(t *testing.T) {
	cls := &fakeClassifier{score: 0.8, isSpeech: true}
	d := NewEnergyDetector(
		WithDetectorClassifier(cls),
		WithDetectorConfirm(1),
	)

	f := makePCMFrame(160, 1000)
	action, seed := d.Detect(f)
	if action != SpeechCandidate {
		t.Fatalf("expected SpeechCandidate, got %v", action)
	}
	if len(seed) == 0 {
		t.Fatal("expected non-empty seed frames")
	}
}

func makeStereoFrame(samples int, amp int16) audio.Frame {
	b := make([]byte, samples*4)
	for i := 0; i < samples*2; i++ {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(amp))
	}
	return audio.Frame{
		Data:   b,
		Format: audio.Format{Codec: audio.CodecPCM, SampleRate: 16000, Channels: 2, BitDepth: 16},
	}
}
