package main

import (
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/voice"
	speechmetrics "github.com/GizClaw/flowcraft/voice/metrics"
	tea "github.com/charmbracelet/bubbletea"
)

// tuiBridge forwards speech metrics and events into the bubbletea program.
// program is set after tea.NewProgram so handlers remain no-ops until then.
type tuiBridge struct {
	program *tea.Program
}

func (b *tuiBridge) metricsHook() speechmetrics.HookFunc {
	return func(m speechmetrics.TurnMetrics) {
		if b.program == nil {
			return
		}
		status := "success"
		if m.Interrupted {
			status = fmt.Sprintf("interrupted (%s)", m.InterruptReason)
		}
		b.program.Send(appendLineMsg{
			role: "status",
			text: fmt.Sprintf("[metrics] %s | e2e=%s stt=%s runner=%s tts=%s play=%s",
				status, m.EndToEnd.Round(time.Millisecond),
				m.STTFinal.Round(time.Millisecond), m.RunnerFirstToken.Round(time.Millisecond),
				m.TTSFirstAudio.Round(time.Millisecond), m.PlaybackTotal.Round(time.Millisecond)),
		})
	}
}

func (b *tuiBridge) speechEventHandler() func(voice.Event) {
	return func(ev voice.Event) {
		if b.program == nil {
			return
		}
		switch ev.Type {
		case voice.EventTurnStarted:
			b.program.Send(appendLineMsg{role: "status", text: "─── turn started ───"})

		case voice.EventTranscriptPartial:
			b.program.Send(updatePartialMsg(ev.Text))

		case voice.EventTranscriptFinal:
			b.program.Send(clearPartialMsg{})
			b.program.Send(appendLineMsg{role: "user", text: ev.Text})

		case voice.EventAudio:
			if ev.Text != "" {
				b.program.Send(appendAIDeltaMsg(ev.Text))
			}

		case voice.EventTurnInterrupted:
			b.program.Send(flushAIStreamMsg{})
			b.program.Send(appendLineMsg{role: "status", text: fmt.Sprintf("─── interrupted (%s) ───", ev.InterruptReason)})

		case voice.EventTurnDone, voice.EventDone:
			b.program.Send(flushAIStreamMsg{})
			b.program.Send(appendLineMsg{role: "status", text: "─── turn complete ───"})

		case voice.EventError:
			b.program.Send(appendLineMsg{role: "status", text: fmt.Sprintf("error [%s]: %s", ev.ErrorCode, ev.Text)})
		}
	}
}
