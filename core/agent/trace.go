package agent

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/core/inference/usage"
)

// RunRecord is one finished run as a trace exporter sees it.
//
// It is the join key between a trace and a dataset row: the span identifies the
// run, the manifest identifies the version combination, and the usage totals
// make the cost comparable across runs.
type RunRecord struct {
	TraceID   string      `json:"trace_id,omitempty"`
	SpanID    string      `json:"span_id,omitempty"`
	SessionID string      `json:"session_id"`
	TaskID    string      `json:"task_id"`
	Manifest  string      `json:"manifest,omitempty"`
	State     TaskState   `json:"state"`
	ErrorKind string      `json:"error_kind,omitempty"`
	LatencyMs int64       `json:"latency_ms"`
	Usage     usage.Usage `json:"usage,omitzero"`
}

// addUsage folds one call's accounting into the run's totals. Token counts,
// cache counters and cost are summed; counters a provider reports only in
// aggregate stay on the last event.
func addUsage(total *usage.Usage, add usage.Usage) {
	total.InputTokens += add.InputTokens
	total.OutputTokens += add.OutputTokens
	total.TotalTokens += add.TotalTokens
	total.LatencyMs += add.LatencyMs
	total.Input.CacheReadTokens = addCounter(total.Input.CacheReadTokens, add.Input.CacheReadTokens)
	total.Input.CacheWriteTokens = addCounter(total.Input.CacheWriteTokens, add.Input.CacheWriteTokens)
	if add.Billing == nil || add.Billing.Cost == nil {
		return
	}
	if total.Billing == nil {
		total.Billing = &usage.BillingUsage{}
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

func addCounter(total, add *int64) *int64 {
	if add == nil {
		return total
	}
	if total == nil {
		value := *add
		return &value
	}
	value := *total + *add
	return &value
}

// errorKind classifies a run failure for the record. The full error stays in
// the event stream; the record carries a stable, low-cardinality label.
func errorKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	default:
		return "error"
	}
}
