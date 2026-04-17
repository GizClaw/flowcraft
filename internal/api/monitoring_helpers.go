package api

import (
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
)

var monitoringWindowDurations = map[string]time.Duration{
	"1h":  time.Hour,
	"6h":  6 * time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
}

func parseMonitoringWindowStrict(window string) (time.Duration, time.Time, error) {
	if window == "" {
		window = "24h"
	}
	d, ok := monitoringWindowDurations[window]
	if !ok {
		return 0, time.Time{}, fmt.Errorf("invalid window: %q", window)
	}
	return d, time.Now().UTC().Add(-d), nil
}

func defaultIntervalForMonitoringWindow(window string) time.Duration {
	switch window {
	case "1h":
		return time.Minute
	case "6h":
		return 5 * time.Minute
	case "7d":
		return time.Hour
	default:
		return 15 * time.Minute
	}
}

func parseMonitoringIntervalStrict(window, interval string) (time.Duration, error) {
	if interval == "" {
		return defaultIntervalForMonitoringWindow(window), nil
	}
	allowed := map[string]time.Duration{
		"1m":  time.Minute,
		"5m":  5 * time.Minute,
		"15m": 15 * time.Minute,
		"1h":  time.Hour,
	}
	d, ok := allowed[interval]
	if !ok {
		return 0, fmt.Errorf("invalid interval: %q", interval)
	}
	return d, nil
}

func clampMonitoringLimit(limit int) int {
	if limit <= 0 {
		return 20
	}
	if limit > 200 {
		return 200
	}
	return limit
}

func classifyMonitoringHealth(summary *model.MonitoringSummary, cfg MonitoringConfig, hasRecentFailureWithoutSuccess bool) {
	if summary.RunTotal == 0 {
		summary.Health = model.MonitoringHealthHealthy
		summary.HealthReason = "no traffic in selected window"
		return
	}
	if summary.ErrorRate != nil && *summary.ErrorRate >= cfg.ErrorRateDown {
		summary.Health = model.MonitoringHealthDown
		summary.HealthReason = fmt.Sprintf("error rate >= %.2f%%", cfg.ErrorRateDown*100)
		return
	}
	if hasRecentFailureWithoutSuccess {
		summary.Health = model.MonitoringHealthDown
		summary.HealthReason = fmt.Sprintf("no successful runs in recent %d minutes", cfg.NoSuccessDownMinutes)
		return
	}
	if summary.ErrorRate != nil && *summary.ErrorRate >= cfg.ErrorRateWarn {
		summary.Health = model.MonitoringHealthDegraded
		summary.HealthReason = fmt.Sprintf("error rate >= %.2f%%", cfg.ErrorRateWarn*100)
		return
	}
	if summary.LatencyP95Ms != nil && *summary.LatencyP95Ms >= float64(cfg.LatencyP95WarnMs) {
		summary.Health = model.MonitoringHealthDegraded
		summary.HealthReason = fmt.Sprintf("p95 latency >= %dms", cfg.LatencyP95WarnMs)
		return
	}
	summary.Health = model.MonitoringHealthHealthy
}
