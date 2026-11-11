package memory

import (
	"context"
	"testing"

	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestAssemblyMetricsRecordContextCounts pins the documented counters:
// memory.context.requests and memory.context.items must reach the configured
// meter reader.
func TestAssemblyMetricsRecordContextCounts(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	shutdown, err := telemetry.InitMeter(ctx, telemetry.WithMeterReader(reader))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := shutdown(ctx); err != nil {
			t.Errorf("telemetry shutdown: %v", err)
		}
	}()

	assembly, _ := newTestAssembly(t, "")
	if err := assembly.CommitTurn(ctx, corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "hello")},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := assembly.Context(ctx, corememory.ContextRequest{Scope: testScope(), ConversationID: "conv-1"})
	if err != nil {
		t.Fatal(err)
	}

	var resources metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &resources); err != nil {
		t.Fatal(err)
	}
	if requests := collectedSum(t, resources, "memory.context.requests"); requests != 1 {
		t.Fatalf("memory.context.requests = %d, want 1", requests)
	}
	if items := collectedSum(t, resources, "memory.context.items"); items != int64(len(result.Items)) {
		t.Fatalf("memory.context.items = %d, want %d", items, len(result.Items))
	}
}

func collectedSum(t *testing.T, resources metricdata.ResourceMetrics, name string) int64 {
	t.Helper()
	for _, scope := range resources.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != name {
				continue
			}
			sum, ok := metric.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %s data is %T, want an int64 sum", name, metric.Data)
			}
			total := int64(0)
			for _, point := range sum.DataPoints {
				total += point.Value
			}
			return total
		}
	}
	t.Fatalf("metric %s was not collected", name)
	return 0
}
