package kanban_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestMetrics_CarryProducerDimension guards the producer attribute from
// becoming a no-op the way AttrKanbanCardKind did. The vocabulary
// constant existed for releases while nothing actually attached it to a
// recording, so any dashboard filter on producer came back empty. We
// install a manual-reader MeterProvider, submit from a known producer
// context, and confirm the submitted/terminal counters plus the
// latency histogram each carry the producer dimension.
func TestMetrics_CarryProducerDimension(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	k := kanban.New("scope-prodtest", kanban.WithMaxPending(4))
	t.Cleanup(func() { _ = k.Close() })

	ctx := kanban.WithProducerID(context.Background(), "agent-orchestrator")
	card, err := k.Submit(ctx, kanban.Task{TargetAgentID: "builder", Query: "go"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !k.Claim(card.ID, "worker-1") {
		t.Fatalf("Claim returned false")
	}
	if !k.Done(card.ID, kanban.Result{Output: "ok"}) {
		t.Fatalf("Done returned false")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	want := map[string]bool{
		"kanban.cards.submitted.total": false,
		"kanban.cards.terminal.total":  false,
		"kanban.cards.latency.seconds": false,
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if _, ok := want[m.Name]; !ok {
				continue
			}
			if datapointHasProducer(m.Data, "agent-orchestrator") {
				want[m.Name] = true
			}
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("metric %s did not record producer=%q; producer is no longer a dimension", name, "agent-orchestrator")
		}
	}
}

// datapointHasProducer checks every datapoint in the aggregation for
// the expected producer. Counter sums and latency histograms are the
// only aggregation kinds this package emits.
func datapointHasProducer(d metricdata.Aggregation, want string) bool {
	switch x := d.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range x.DataPoints {
			if v, ok := dp.Attributes.Value(telemetry.AttrKanbanProducerID); ok && v.AsString() == want {
				return true
			}
		}
	case metricdata.Histogram[float64]:
		for _, dp := range x.DataPoints {
			if v, ok := dp.Attributes.Value(telemetry.AttrKanbanProducerID); ok && v.AsString() == want {
				return true
			}
		}
	}
	return false
}
