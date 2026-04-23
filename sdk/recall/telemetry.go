package recall

import (
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"go.opentelemetry.io/otel/metric"
)

// Meter naming follows the convention established by sdk/history
// (memory.archive, memory.dag, …). Suffixed under "memory.recall" so a
// single OTel pipeline can group all long-term memory subsystems with
// one selector.
//
// Histograms are in seconds (OTel convention); counters carry no unit
// and label cardinality is bounded by enum-like values (state /
// outcome) — never user-supplied scope fields, which would explode the
// metric series.
var (
	recallMeter = telemetry.MeterWithSuffix("memory.recall")

	addDuration, _    = recallMeter.Float64Histogram("add_duration", metric.WithDescription("recall.Add duration in seconds"))
	addTotal, _       = recallMeter.Int64Counter("add_total", metric.WithDescription("recall.Add invocations, labelled by outcome"))
	recallDuration, _ = recallMeter.Float64Histogram("recall_duration", metric.WithDescription("recall.Recall duration in seconds"))
	recallTotal, _    = recallMeter.Int64Counter("recall_total", metric.WithDescription("recall.Recall invocations, labelled by outcome"))
	recallHits, _     = recallMeter.Int64Histogram("recall_hits", metric.WithDescription("Number of hits returned per recall.Recall call"))

	jobDuration, _    = recallMeter.Float64Histogram("job_duration", metric.WithDescription("Async Save job duration in seconds (extract + upsert)"))
	jobTotal, _       = recallMeter.Int64Counter("job_total", metric.WithDescription("Async Save jobs processed, labelled by outcome (succeeded/failed/dead/timeout)"))
	jobLeaseErrors, _ = recallMeter.Int64Counter("job_lease_errors_total", metric.WithDescription("JobQueue.Lease errors observed by the worker loop (excluding shutdown cancellations)"))
)
