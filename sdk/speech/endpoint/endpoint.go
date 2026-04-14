package endpoint

import "time"

type Decision int

const (
	Continue Decision = iota
	Commit
)

type Input struct {
	IsSpeech       bool
	ExplicitCommit bool
}

type Decider interface {
	Feed(input Input) Decision
	Reset()
}

type SilenceDecider struct {
	silentN int
	limit   int
}

func NewSilenceDecider(silenceDuration, frameSize time.Duration) *SilenceDecider {
	limit := 1
	if frameSize > 0 && silenceDuration > 0 {
		limit = max(int(silenceDuration/frameSize), 1)
	}
	return &SilenceDecider{limit: limit}
}

func (d *SilenceDecider) Feed(input Input) Decision {
	if input.ExplicitCommit {
		d.Reset()
		return Commit
	}
	if input.IsSpeech {
		d.silentN = 0
		return Continue
	}
	d.silentN++
	if d.silentN >= d.limit {
		d.Reset()
		return Commit
	}
	return Continue
}

func (d *SilenceDecider) Reset() {
	d.silentN = 0
}
