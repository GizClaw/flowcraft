package app

import "github.com/GizClaw/flowcraft/core/inference/usage"

// Usage is the token accounting of one turn.
//
// It is folded from the run's Usage events, not from a counter the runtime keeps:
// the stream is the report, and a snapshot taken after the fact would have to
// trust a second source. Counters a provider reports only in aggregate stay on
// the last event, so they are summed here rather than diffed.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	ReasoningTokens  int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	Calls            int64
}

// Add folds one round's report in.
func (u *Usage) Add(report usage.Usage) {
	u.InputTokens += report.InputTokens
	u.OutputTokens += report.OutputTokens
	u.Calls++
	if report.Details.Output.ReasoningTokens != nil {
		u.ReasoningTokens += *report.Details.Output.ReasoningTokens
	}
	if report.Input.CacheReadTokens != nil {
		u.CacheReadTokens += *report.Input.CacheReadTokens
	}
	if report.Input.CacheWriteTokens != nil {
		u.CacheWriteTokens += *report.Input.CacheWriteTokens
	}
}

// Total is the token count a person reads: input plus output.
func (u Usage) Total() int64 { return u.InputTokens + u.OutputTokens }
