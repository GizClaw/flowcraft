package bootstrap

import "github.com/GizClaw/flowcraft/internal/eventlog"

// WireRetention starts the retention goroutine using the default category
// TTLs. The returned cancel function stops the goroutine on shutdown.
//
// In R2 the goroutine ticks but does not delete; R3 enables actual deletion.
// We still wire it now so the goroutine lifecycle (start/stop) is exercised
// in tests.
func WireRetention(log *eventlog.SQLiteLog) func() {
	return eventlog.StartRetentionGoroutine(log, eventlog.DefaultRetentionConfig, log.Checkpoints())
}
