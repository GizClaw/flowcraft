package model

import "time"

type MonitoringHealthStatus string

const (
	MonitoringHealthHealthy  MonitoringHealthStatus = "healthy"
	MonitoringHealthDegraded MonitoringHealthStatus = "degraded"
	MonitoringHealthDown     MonitoringHealthStatus = "down"
)

type MonitoringSummary struct {
	WindowStart     time.Time              `json:"window_start"`
	WindowEnd       time.Time              `json:"window_end"`
	RunTotal        int64                  `json:"run_total"`
	RunSuccess      int64                  `json:"run_success"`
	RunFailed       int64                  `json:"run_failed"`
	SuccessRate     *float64               `json:"success_rate"`
	ErrorRate       *float64               `json:"error_rate"`
	LatencyP50Ms    *float64               `json:"latency_p50_ms"`
	LatencyP95Ms    *float64               `json:"latency_p95_ms"`
	LatencyP99Ms    *float64               `json:"latency_p99_ms"`
	Health          MonitoringHealthStatus `json:"health"`
	HealthReason    string                 `json:"health_reason,omitempty"`
	ActiveActors    int                    `json:"active_actors"`
	ActiveSandboxes int                    `json:"active_sandboxes"`
	Thresholds      MonitoringThresholds   `json:"thresholds"`
}

type MonitoringThresholds struct {
	ErrorRateWarn        float64 `json:"error_rate_warn"`
	ErrorRateDown        float64 `json:"error_rate_down"`
	LatencyP95WarnMs     int64   `json:"latency_p95_warn_ms"`
	ConsecutiveBuckets   int     `json:"consecutive_buckets"`
	NoSuccessDownMinutes int     `json:"no_success_down_minutes"`
}

type MonitoringTimeseriesPoint struct {
	BucketStart   time.Time `json:"bucket_start"`
	RunTotal      int64     `json:"run_total"`
	RunSuccess    int64     `json:"run_success"`
	RunFailed     int64     `json:"run_failed"`
	SuccessRate   *float64  `json:"success_rate"`
	ErrorRate     *float64  `json:"error_rate"`
	LatencyP50Ms  *float64  `json:"latency_p50_ms"`
	LatencyP95Ms  *float64  `json:"latency_p95_ms"`
	LatencyP99Ms  *float64  `json:"latency_p99_ms"`
	AvgElapsedMs  *float64  `json:"avg_elapsed_ms"`
	ThroughputRPM float64   `json:"throughput_rpm"`
}

type MonitoringTopFailedAgent struct {
	AgentID     string   `json:"agent_id"`
	FailedRuns  int64    `json:"failed_runs"`
	TotalRuns   int64    `json:"total_runs"`
	FailureRate *float64 `json:"failure_rate"`
}

type MonitoringTopErrorCode struct {
	Code  string `json:"code"`
	Count int64  `json:"count"`
}

type MonitoringRecentFailure struct {
	RunID     string    `json:"run_id"`
	AgentID   string    `json:"agent_id"`
	ErrorCode string    `json:"error_code"`
	Message   string    `json:"message"`
	ElapsedMs int64     `json:"elapsed_ms"`
	CreatedAt time.Time `json:"created_at"`
}

type MonitoringDiagnostics struct {
	TopFailedAgents []MonitoringTopFailedAgent `json:"top_failed_agents"`
	TopErrorCodes   []MonitoringTopErrorCode   `json:"top_error_codes"`
	RecentFailures  []MonitoringRecentFailure  `json:"recent_failures"`
}
