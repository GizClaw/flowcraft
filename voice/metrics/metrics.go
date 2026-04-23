package metrics

import (
	"time"

	"github.com/GizClaw/flowcraft/voice/provider"
)

type TurnMetrics struct {
	SessionID string
	TurnID    string
	RunID     string

	StartedAt   time.Time
	CompletedAt time.Time

	STTFirstPartial  time.Duration
	STTFinal         time.Duration
	RunnerFirstToken time.Duration
	TTSFirstAudio    time.Duration
	PlaybackTotal    time.Duration
	EndToEnd         time.Duration

	STTProviderReport provider.Report
	TTSProviderReport provider.Report

	Interrupted     bool
	InterruptReason string
}

type Hook interface {
	OnTurnMetrics(TurnMetrics)
}

type HookFunc func(TurnMetrics)

func (f HookFunc) OnTurnMetrics(m TurnMetrics) { f(m) }
