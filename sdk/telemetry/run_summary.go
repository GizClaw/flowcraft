package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// RunSummary captures the outcome of one engine.Run for telemetry
// purposes. It is intentionally engine-neutral and OTel-SDK-shaped (no
// dependency on sdk/model or sdk/engine) so this helper can stay in
// the leaf telemetry package.
//
// All fields are optional. RecordRunSummary fills sensible defaults
// (Status="ok" when empty, Duration computed from StartedAt when
// EndedAt is zero, …) and tolerates a fully zero value (it just
// records the bare minimum span).
type RunSummary struct {
	// RunID is the engine.Run.ID (or any other stable per-execution
	// identifier the producer uses). Omitted from span attributes
	// when empty.
	RunID string

	// ParentRunID, when non-empty, identifies the calling run in a
	// multi-agent dispatch chain.
	ParentRunID string

	// AgentID identifies the agent that owned the run (sdk/agent.Agent.ID).
	AgentID string

	// PodID identifies the sdk/pod runtime instance, when applicable.
	PodID string

	// EngineKind is a short stable token for the executing engine
	// implementation ("graph" / "script" / "a2a-remote" / ...).
	EngineKind string

	// Status reports the terminal outcome. Recommended values are
	// the same as documented on AttrRunStatus: "ok", "interrupted",
	// "cancelled", "failed". Empty defaults to "ok".
	Status string

	// Err is the error returned by Engine.Execute, if any. When
	// non-nil the span status is set to codes.Error and Err.Error()
	// is recorded; Status defaults to "failed" if not set
	// explicitly by the caller.
	Err error

	// StartedAt / EndedAt bracket the execution wall clock. When
	// EndedAt is zero it defaults to time.Now(); when StartedAt is
	// zero the resulting span has zero duration but is still
	// emitted.
	StartedAt time.Time
	EndedAt   time.Time

	// LLMModel, when non-empty, identifies the dominant model used
	// in the run (typically the only one). Producers handling
	// multi-model runs SHOULD emit one summary per model or pick
	// the most-used one.
	LLMModel string

	// Token / cost / latency totals, mirroring the AttrLLM* keys.
	// Zero values are omitted from the span.
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	CostMicros   int64

	// Extra carries additional caller-supplied attributes that
	// should land on the same span (tenant id, custom dimensions,
	// …). Use the Attr* constants when applicable.
	Extra []attribute.KeyValue
}

// RecordRunSummary emits a short-lived "engine.run.summary" span
// summarising one engine.Run. The span is started and ended
// synchronously inside this call — its only job is to carry the
// summary attributes; no real work happens between Start and End.
//
// Why a span and not a metric or log:
//
//   - it inherits the active TraceID from ctx, so dashboards can drill
//     from the per-run summary back to the per-step spans the engine
//     emitted during execution without separate correlation logic;
//   - exporters route it through the existing OTLP / file / stdout
//     pipeline configured by InitTracer — no new sink to wire;
//   - duration is a first-class span attribute, no extra attribute key
//     needed.
//
// When ctx carries no active TracerProvider this still creates a span
// against the global noop tracer — the call is a no-op but always
// safe.
//
// RecordRunSummary is the lowest-level helper; sdk/agent and sdk/pod
// will likely wrap it with their own typed entry points (e.g. one
// that takes an agent.Result and pre-fills RunID / Status / Err).
func RecordRunSummary(ctx context.Context, summary RunSummary) {
	if ctx == nil {
		ctx = context.Background()
	}

	startedAt := summary.StartedAt
	endedAt := summary.EndedAt
	if endedAt.IsZero() {
		endedAt = time.Now()
	}
	if startedAt.IsZero() {
		startedAt = endedAt
	}

	status := summary.Status
	if status == "" {
		if summary.Err != nil {
			status = "failed"
		} else {
			status = "ok"
		}
	}

	attrs := make([]attribute.KeyValue, 0, 12+len(summary.Extra))
	if summary.RunID != "" {
		attrs = append(attrs, attribute.String(AttrRunID, summary.RunID))
	}
	if summary.ParentRunID != "" {
		attrs = append(attrs, attribute.String(AttrParentRunID, summary.ParentRunID))
	}
	if summary.AgentID != "" {
		attrs = append(attrs, attribute.String(AttrAgentID, summary.AgentID))
	}
	if summary.PodID != "" {
		attrs = append(attrs, attribute.String(AttrPodID, summary.PodID))
	}
	if summary.EngineKind != "" {
		attrs = append(attrs, attribute.String(AttrEngineKind, summary.EngineKind))
	}
	attrs = append(attrs, attribute.String(AttrRunStatus, status))

	if summary.LLMModel != "" {
		attrs = append(attrs, attribute.String(AttrLLMModel, summary.LLMModel))
	}
	if summary.InputTokens > 0 {
		attrs = append(attrs, attribute.Int64(AttrLLMInputTokens, summary.InputTokens))
	}
	if summary.OutputTokens > 0 {
		attrs = append(attrs, attribute.Int64(AttrLLMOutputTokens, summary.OutputTokens))
	}
	if summary.TotalTokens > 0 {
		attrs = append(attrs, attribute.Int64(AttrLLMTotalTokens, summary.TotalTokens))
	}
	if summary.CostMicros > 0 {
		attrs = append(attrs, attribute.Int64(AttrLLMCostMicros, summary.CostMicros))
	}

	if !startedAt.IsZero() && !endedAt.IsZero() && endedAt.After(startedAt) {
		latencyMs := endedAt.Sub(startedAt).Milliseconds()
		attrs = append(attrs, attribute.Int64(AttrLLMLatencyMs, latencyMs))
	}

	attrs = append(attrs, summary.Extra...)

	_, span := Tracer().Start(ctx, "engine.run.summary",
		trace.WithTimestamp(startedAt),
		trace.WithAttributes(attrs...),
	)
	if summary.Err != nil {
		span.RecordError(summary.Err)
		span.SetStatus(codes.Error, summary.Err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End(trace.WithTimestamp(endedAt))
}
