package kanban_test

import (
	"context"
	"testing"
	"time"

	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdkx/delegation/kanban"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestEventsCarryTypedDelegationAndLocalHeaders(t *testing.T) {
	board := newBoard(t)
	subscription, err := board.Bus().Subscribe(
		context.Background(), kanban.PatternAll(), event.WithBufferSize(8))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer subscription.Close()

	id := submit(t, board, "worker")
	work, _ := board.Claim(context.Background())
	_ = board.Complete(context.Background(), id, work.LeaseToken, sdkdelegation.Response{
		Status: sdkdelegation.StatusFailed,
		Error:  "boom",
	})

	want := []struct {
		kind   string
		status kanban.Status
	}{
		{kanban.EventCardSubmitted, kanban.StatusPending},
		{kanban.EventCardClaimed, kanban.StatusClaimed},
		{kanban.EventCardFailed, kanban.StatusFailed},
	}
	for _, expected := range want {
		select {
		case envelope := <-subscription.C():
			if envelope.Header(kanban.HeaderKind) != expected.kind ||
				envelope.Header(kanban.HeaderCardID) != id ||
				kanban.KanbanScopeID(envelope) != "test-backend" {
				t.Fatalf("event headers = %+v", envelope.Headers)
			}
			var payload kanban.CardEvent
			if err := envelope.Decode(&payload); err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if payload.Status != expected.status ||
				payload.Request == nil ||
				payload.Request.Request.Target != "worker" {
				t.Fatalf("event payload = %+v", payload)
			}
			if expected.status == kanban.StatusFailed &&
				(payload.Response == nil ||
					payload.Response.Status != sdkdelegation.StatusFailed ||
					payload.Response.Error != "boom") {
				t.Fatalf("terminal payload = %+v", payload)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for event")
		}
	}
}

func TestEventPatternSanitizesCardID(t *testing.T) {
	if got, want := kanban.PatternCard("a.b*c>d"),
		event.Pattern("delegation.kanban.card.a_b_c_d.>"); got != want {
		t.Fatalf("PatternCard = %q, want %q", got, want)
	}
}

func TestMetricsCarryBoundedDelegationDimensions(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(previous) })

	board := newBoard(t)
	id := submit(t, board, "worker")
	work, _ := board.Claim(context.Background())
	_ = board.Complete(context.Background(), id, work.LeaseToken, sdkdelegation.Response{
		Status: sdkdelegation.StatusSucceeded,
		Output: "ok",
	})

	var resources metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &resources); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]bool{
		"delegation.kanban.cards.submitted.total": false,
		"delegation.kanban.cards.terminal.total":  false,
		"delegation.kanban.cards.latency.seconds": false,
	}
	for _, scope := range resources.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if _, ok := want[metric.Name]; !ok {
				continue
			}
			if hasMetricDimensions(metric.Data, "planner", "worker") {
				want[metric.Name] = true
			}
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("metric %s lacks delegation dimensions", name)
		}
	}
}

func hasMetricDimensions(
	aggregation metricdata.Aggregation,
	producer string,
	target string,
) bool {
	switch data := aggregation.(type) {
	case metricdata.Sum[int64]:
		for _, point := range data.DataPoints {
			if dimensionsMatch(point.Attributes, producer, target) {
				return true
			}
		}
	case metricdata.Histogram[float64]:
		for _, point := range data.DataPoints {
			if dimensionsMatch(point.Attributes, producer, target) {
				return true
			}
		}
	}
	return false
}

func dimensionsMatch(
	attributes attribute.Set,
	producer string,
	target string,
) bool {
	producerID, producerOK := attributes.Value(kanban.AttrKanbanProducerID)
	targetID, targetOK := attributes.Value(kanban.AttrKanbanTargetAgentID)
	_, cardIDPresent := attributes.Value("kanban.card.id")
	_, delegationIDPresent := attributes.Value("delegation.id")
	return !cardIDPresent && !delegationIDPresent && producerOK && targetOK &&
		producerID.AsString() == producer &&
		targetID.AsString() == target
}
