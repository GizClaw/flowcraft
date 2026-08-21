package graph

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// Kernel instrumentation, following the core/inference pattern: a
// package-level meter under the "graph" instrumentation scope, with
// spans created per Execute / node invocation by the executor.
//
// Metric attribute conventions:
//   - status: "success" / "error" / "interrupted" / "skipped"
//   - identity: telemetry.AttrGraphName / AttrNodeID / AttrRunID /
//     AttrEngineKind
//
// Subjects themselves carry run ids and are unsuitable as metric
// attributes; publish failures use the low-cardinality "event.kind"
// classes "step" / "stream_delta".

// engineKind is the stable engine-kind token for the graph runner,
// matching the "graph" registry key used by core/graph/config.
const engineKind = "graph"

var (
	graphMeter = telemetry.MeterWithSuffix("graph")

	graphExecCount, _ = graphMeter.Int64Counter(
		"executions.total",
		metric.WithDescription("Total graph executions by status"))
	graphExecDuration, _ = graphMeter.Float64Histogram(
		"duration.seconds",
		metric.WithDescription("Graph execution duration"))
	nodeExecCount, _ = graphMeter.Int64Counter(
		"node.executions.total",
		metric.WithDescription("Total node invocations by status"))
	nodeExecDuration, _ = graphMeter.Float64Histogram(
		"node.duration.seconds",
		metric.WithDescription("Node invocation duration"))
	graphPublishErrors, _ = graphMeter.Int64Counter(
		"publish_errors.total",
		metric.WithDescription(
			"Event publish failures swallowed by the graph kernel (best-effort observability events)"))
)

// runScope returns the run-level scope dimensions shared by spans and
// log records: parent run, task, conversation, tenant and engine kind.
// Empty identity dimensions are omitted so payloads stay slim; engine
// kind is always present. Keeping one source of truth prevents the
// span and log attribute sets from drifting.
func runScope(run agent.Run) []runScopeKV {
	var kvs []runScopeKV
	if run.ParentRunID != "" {
		kvs = append(kvs, runScopeKV{telemetry.AttrParentRunID, run.ParentRunID})
	}
	if run.TaskID != "" {
		kvs = append(kvs, runScopeKV{telemetry.AttrTaskID, run.TaskID})
	}
	if run.ConversationID != "" {
		kvs = append(kvs, runScopeKV{telemetry.AttrConversationID, run.ConversationID})
	}
	if tenantID := run.Attributes[telemetry.AttrTenantID]; tenantID != "" {
		kvs = append(kvs, runScopeKV{telemetry.AttrTenantID, tenantID})
	}
	return append(kvs, runScopeKV{telemetry.AttrEngineKind, engineKind})
}

type runScopeKV struct {
	key   string
	value string
}

// runScopeAttrs converts the shared run scope into span attributes.
func runScopeAttrs(run agent.Run) []attribute.KeyValue {
	kvs := runScope(run)
	attrs := make([]attribute.KeyValue, 0, len(kvs))
	for _, kv := range kvs {
		attrs = append(attrs, attribute.String(kv.key, kv.value))
	}
	return attrs
}

// runScopeLogAttrs converts the shared run scope into log attributes.
func runScopeLogAttrs(run agent.Run) []otellog.KeyValue {
	kvs := runScope(run)
	attrs := make([]otellog.KeyValue, 0, len(kvs))
	for _, kv := range kvs {
		attrs = append(attrs, otellog.String(kv.key, kv.value))
	}
	return attrs
}

// recordGraphExec records one completed Execute call.
func recordGraphExec(ctx context.Context, g *Graph, run agent.Run, status string, dur time.Duration) {
	attrs := metric.WithAttributes(
		attribute.String(telemetry.AttrGraphName, g.name),
		attribute.String(telemetry.AttrRunID, run.RunID),
		attribute.String(telemetry.AttrAgentID, run.AgentID),
		attribute.String(telemetry.AttrEngineKind, engineKind),
		attribute.String("status", status),
	)
	graphExecCount.Add(ctx, 1, attrs)
	graphExecDuration.Record(ctx, dur.Seconds(), attrs)
}

// recordNodeExec records one completed node invocation.
func recordNodeExec(ctx context.Context, g *Graph, slot *nodeSlot, run agent.Run, status string, dur time.Duration) {
	attrs := metric.WithAttributes(
		attribute.String(telemetry.AttrGraphName, g.name),
		attribute.String(telemetry.AttrNodeID, slot.def.ID),
		attribute.String("node.type", slot.def.Type),
		attribute.String("status", status),
	)
	nodeExecCount.Add(ctx, 1, attrs)
	nodeExecDuration.Record(ctx, dur.Seconds(), attrs)
}

// recordPublishError counts one swallowed publish failure (see
// publishStep / publishBranchDelta). Swallowing keeps observability
// events from distorting execution; counting keeps bus
// misconfigurations and flaky subscribers visible.
func recordPublishError(ctx context.Context, kind string, info agent.RunInfo, nodeID string) {
	graphPublishErrors.Add(ctx, 1, metric.WithAttributes(
		attribute.String("event.kind", kind),
		attribute.String(telemetry.AttrNodeID, nodeID),
		attribute.String(telemetry.AttrRunID, info.RunID),
	))
}

// execStatus classifies an Execute outcome for metrics and spans.
func execStatus(err error) string {
	switch {
	case err == nil:
		return "success"
	case isInterruptedErr(err):
		return "interrupted"
	default:
		return "error"
	}
}

// runStatusValue maps the engine's outcome vocabulary to the
// canonical telemetry values documented on AttrRunStatus.
func runStatusValue(status string) string {
	switch status {
	case "success":
		return "ok"
	case "interrupted":
		return "interrupted"
	default:
		return "failed"
	}
}

func isInterruptedErr(err error) bool {
	return err != nil && errdefs.IsInterrupted(err)
}
