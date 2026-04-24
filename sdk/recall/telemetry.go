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

	// recallStageDuration tracks per-stage timings of the retrieval
	// pipeline executed under recall.Recall / recall.RecallExplain. The
	// stage label is the stage name reported by pipeline.Stage.Name()
	// (a small, enum-like set bounded by the configured pipeline), so
	// cardinality stays controlled. Use this histogram to answer "which
	// retrieval stage is currently the slow one" in dashboards without
	// having to enable per-call SearchDebug.
	recallStageDuration, _ = recallMeter.Float64Histogram(
		"recall_stage_duration",
		metric.WithDescription("Per-stage duration in seconds for the retrieval pipeline executed under recall.Recall, labelled by stage name and outcome"),
	)

	// recallLaneDuration tracks per-lane recall timings within
	// MultiRetrieve / Retrieve stages. The lane label is the canonical
	// retrieval.LaneKey (bm25/vector/sparse/entity/...), which is a
	// small, enum-like set bounded by the configured pipeline. Pair
	// with recallStageDuration to answer both "which stage is slow"
	// (coarse) and "which recall lane is slow" (fine) without enabling
	// per-call SearchDebug.
	recallLaneDuration, _ = recallMeter.Float64Histogram(
		"recall_lane_duration",
		metric.WithDescription("Per-lane recall duration in seconds inside the retrieval pipeline executed under recall.Recall, labelled by lane key"),
	)
)
