package detect

import (
	"github.com/GizClaw/flowcraft/sdk/speech/audio"
	"github.com/GizClaw/flowcraft/sdk/speech/vad"
)

// EnergyDetectorOption configures an EnergyDetector.
type EnergyDetectorOption func(*EnergyDetector)

// WithDetectorThreshold sets the speech-onset threshold.
// In classifier mode this is a score in [0,1]; otherwise it is an RMS value.
// Default: 0.008 (RMS) or 0.45 (classifier).
func WithDetectorThreshold(v float64) EnergyDetectorOption {
	return func(d *EnergyDetector) {
		d.speechThreshold = v
		d.speechThresholdSet = true
	}
}

// WithDetectorInterruptThreshold sets the barge-in threshold (before boost).
// Default: 0.015 (RMS) or 0.55 (classifier).
func WithDetectorInterruptThreshold(v float64) EnergyDetectorOption {
	return func(d *EnergyDetector) {
		d.interruptThreshold = v
		d.interruptThresholdSet = true
	}
}

// WithDetectorBoost sets a multiplier applied to the interrupt threshold,
// making barge-in harder (reduces false positives from echo/feedback).
// Default: 1.0. Typical values: 1.3–2.0.
func WithDetectorBoost(v float64) EnergyDetectorOption {
	return func(d *EnergyDetector) { d.boost = v }
}

// WithDetectorConfirm sets the number of consecutive frames that must
// exceed the interrupt threshold before a barge-in is confirmed.
// Default: 3.
func WithDetectorConfirm(n int) EnergyDetectorOption {
	return func(d *EnergyDetector) { d.confirmN = n }
}

// WithDetectorClassifier sets a Classifier for spectral speech/noise
// discrimination. When set, scores from the classifier are compared against
// the configured thresholds instead of raw RMS energy.
func WithDetectorClassifier(c vad.Classifier) EnergyDetectorOption {
	return func(d *EnergyDetector) {
		d.classifier = c
		if !d.speechThresholdSet {
			d.speechThreshold = 0.45
		}
		if !d.interruptThresholdSet {
			d.interruptThreshold = 0.55
		}
	}
}

// EnergyDetector is a SpeechDetector that uses audio energy (RMS) or
// an optional Classifier to detect speech onset and barge-in.
//
// Speech onset: single frame exceeding speechThreshold.
// Barge-in candidate: confirmN consecutive frames exceeding
// interruptThreshold * boost.
type EnergyDetector struct {
	classifier            vad.Classifier
	speechThreshold       float64
	speechThresholdSet    bool
	interruptThreshold    float64
	interruptThresholdSet bool
	boost                 float64
	confirmN              int

	// mutable state for Detect (barge-in confirmation)
	loudN int
	buf   []audio.Frame
}

// NewEnergyDetector creates an EnergyDetector with the given options.
func NewEnergyDetector(opts ...EnergyDetectorOption) *EnergyDetector {
	d := &EnergyDetector{
		speechThreshold:    0.008,
		interruptThreshold: 0.015,
		boost:              1.0,
		confirmN:           3,
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// IsSpeech returns true if the frame's score exceeds the speech-onset threshold.
func (d *EnergyDetector) IsSpeech(f audio.Frame) bool {
	return d.score(f) >= d.speechThreshold
}

// Detect checks whether the frame constitutes a barge-in candidate.
// It accumulates consecutive loud frames and returns SpeechCandidate with
// the buffered seed frames once confirmN is reached. Session can apply a
// second confirmation layer before executing the interrupt.
func (d *EnergyDetector) Detect(f audio.Frame) (SpeechAction, []audio.Frame) {
	threshold := d.interruptThreshold * d.boost
	if d.score(f) >= threshold {
		d.loudN++
		d.buf = append(d.buf, f)
		if d.loudN >= d.confirmN {
			seed := append([]audio.Frame(nil), d.buf...)
			return SpeechCandidate, seed
		}
	} else {
		d.loudN = 0
		d.buf = nil
	}
	return SpeechNone, nil
}

// Reset clears the barge-in confirmation counters and buffer.
func (d *EnergyDetector) Reset() {
	d.loudN = 0
	d.buf = nil
}

func (d *EnergyDetector) score(f audio.Frame) float64 {
	if d.classifier != nil {
		s, _ := d.classifier.Classify(f.Data)
		return s
	}
	return vad.PCM16RMS(f.Data, 2)
}
