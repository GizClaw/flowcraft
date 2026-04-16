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
	PartialText    string
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
	return &SilenceDecider{limit: framesToLimit(silenceDuration, frameSize)}
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

func framesToLimit(duration, frameSize time.Duration) int {
	if frameSize <= 0 || duration <= 0 {
		return 1
	}
	return max(int((duration+frameSize-1)/frameSize), 1)
}
