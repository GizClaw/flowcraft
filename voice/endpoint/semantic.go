package endpoint

import (
	"strings"
	"time"
	"unicode/utf8"
)

const defaultTerminals = "。！？.!?\n"

// SemanticOption configures a SemanticSilenceDecider.
type SemanticOption func(*SemanticSilenceDecider)

// WithReducedSilence sets the silence duration used when the partial
// transcript ends with a sentence terminator. Default 200ms.
func WithReducedSilence(d time.Duration) SemanticOption {
	return func(s *SemanticSilenceDecider) { s.reducedDuration = d }
}

// WithTerminals sets the characters treated as sentence terminators.
func WithTerminals(chars string) SemanticOption {
	return func(s *SemanticSilenceDecider) { s.terminals = chars }
}

// SemanticSilenceDecider extends silence-based endpoint detection with a
// semantic signal: when the STT partial text ends with a sentence terminator,
// the required silence duration is shortened from fullLimit to reducedLimit.
//
// This avoids the aggressive "zero-silence commit" approach—some silence
// is still required—while shaving 200-300ms off typical turn latency.
type SemanticSilenceDecider struct {
	silentN         int
	fullLimit       int
	reducedLimit    int
	reducedDuration time.Duration
	terminals       string
}

// NewSemanticSilenceDecider creates a decider that reduces the silence
// threshold when the partial transcript appears sentence-complete.
//
// silenceDuration is the normal (full) silence threshold.
// frameSize is the audio frame duration used to convert durations to frame counts.
// Use SemanticOption values to override the reduced silence (default 200ms)
// and terminal characters.
func NewSemanticSilenceDecider(silenceDuration, frameSize time.Duration, opts ...SemanticOption) *SemanticSilenceDecider {
	d := &SemanticSilenceDecider{
		reducedDuration: 200 * time.Millisecond,
		terminals:       defaultTerminals,
	}
	for _, o := range opts {
		o(d)
	}
	d.fullLimit = framesToLimit(silenceDuration, frameSize)
	d.reducedLimit = min(framesToLimit(d.reducedDuration, frameSize), d.fullLimit)
	return d
}

func (d *SemanticSilenceDecider) Feed(input Input) Decision {
	if input.ExplicitCommit {
		d.Reset()
		return Commit
	}
	if input.IsSpeech {
		d.silentN = 0
		return Continue
	}
	d.silentN++
	limit := d.fullLimit
	if d.endsWithTerminal(input.PartialText) {
		limit = d.reducedLimit
	}
	if d.silentN >= limit {
		d.Reset()
		return Commit
	}
	return Continue
}

func (d *SemanticSilenceDecider) Reset() {
	d.silentN = 0
}

func (d *SemanticSilenceDecider) endsWithTerminal(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(text)
	return strings.ContainsRune(d.terminals, r)
}
