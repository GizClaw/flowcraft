package detect

import "github.com/GizClaw/flowcraft/voice/audio"

// SpeechAction is the verdict returned by SpeechDetector.Detect.
type SpeechAction int

const (
	SpeechNone      SpeechAction = iota // no barge-in activity
	SpeechCandidate                     // barge-in candidate; needs second-stage confirmation
	SpeechBargeIn                       // barge-in confirmed; seed frames are valid
)

// SpeechDetector analyzes audio frames to detect speech activity.
//
// It encapsulates all VAD logic—energy thresholds, classifiers, confirmation
// counters, boost factors, and (in advanced implementations) STT-based
// two-stage confirmation. The Session itself does not know how detection works.
type SpeechDetector interface {
	// IsSpeech returns true if the frame contains speech.
	// Used during StateIdle (onset detection) and StateHearing (silence counting).
	// This should be a stateless, side-effect-free check.
	IsSpeech(f audio.Frame) bool

	// Detect analyzes a frame during interruptable states (responding/playback).
	// It maintains internal state (counters, buffers) across consecutive calls.
	// Returns SpeechCandidate for first-stage hits and optionally SpeechBargeIn
	// for detectors that can confirm internally. The Session may still apply a
	// second-stage confirmation policy before executing the interrupt.
	Detect(f audio.Frame) (SpeechAction, []audio.Frame)

	// Reset clears internal state (counters, buffers). Called when the session
	// transitions out of an interruptable state or a barge-in is executed.
	Reset()
}

// PlaybackAwareDetector is an optional extension that accepts playback
// reference frames for echo suppression or AEC-aware decisions.
type PlaybackAwareDetector interface {
	SpeechDetector
	FeedPlayback(f audio.Frame)
}
