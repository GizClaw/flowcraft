package agent

import (
	"time"

	"github.com/GizClaw/flowcraft/core/outcome"
)

// RunRecord is one finished run as a trace exporter sees it.
//
// It is the join key between a trace and a dataset row: the span identifies the
// run, the manifest identifies the version combination, and the usage totals
// make the cost comparable across runs.
type RunRecord struct {
	TraceID string `json:"trace_id,omitempty"`
	SpanID  string `json:"span_id,omitempty"`
	// Agent is the deployment the run belongs to. It is what a dataset row groups by
	// before it joins anything, so it is written here rather than recovered from the
	// span: a sink that only stores rows still has to say which agent produced one.
	Agent     string `json:"agent"`
	SessionID string `json:"session_id"`
	TaskID    string `json:"task_id"`
	TenantID  string `json:"tenant_id,omitempty"`
	// Manifest is [Manifest.ID] of the version combination the run executed under: the
	// join key between two runs that can meaningfully be compared. Empty when the host
	// assembled none.
	Manifest  string    `json:"manifest,omitempty"`
	State     TaskState `json:"state"`
	ErrorKind string    `json:"error_kind,omitempty"`
	// StartedAt is when the run began, the absolute anchor LatencyMs alone cannot
	// provide: a dataset row joins to a span, a log range and a task by time, and
	// "it took four seconds" says nothing about when.
	StartedAt time.Time     `json:"started_at,omitzero"`
	LatencyMs int64         `json:"latency_ms"`
	Usage     outcome.Usage `json:"usage,omitzero"`
}

// addUsage folds one call's accounting into the run's totals. Token counts,
// cache counters and cost are summed; counters a provider reports only in
// aggregate stay on the last event.
func addUsage(total *outcome.Usage, add outcome.Usage) {
	*total = total.AddMeter(add)
	if add.Billing == nil || add.Billing.Cost == nil {
		return
	}
	if total.Billing == nil {
		total.Billing = &outcome.Billing{}
	}
	if total.Billing.Cost == nil {
		cost := *add.Billing.Cost
		total.Billing.Cost = &cost
		return
	}
	if sum, ok := total.Billing.Cost.Add(*add.Billing.Cost); ok {
		total.Billing.Cost = &sum
	}
}
