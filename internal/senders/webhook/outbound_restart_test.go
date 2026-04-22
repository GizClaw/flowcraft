package webhook

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/eventlogtest"
)

func envFromPayload(t *testing.T, etype, partition string, payload any) eventlog.Envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return eventlog.Envelope{
		Type:      etype,
		Partition: partition,
		Payload:   raw,
		Ts:        time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func TestWebhookOutboundSender_ApplyQueuedRebuildsHeap(t *testing.T) {
	log := eventlogtest.NewMemoryLog()
	s := NewWebhookOutboundSender(log, Options{})

	env := envFromPayload(t, eventlog.EventTypeWebhookOutboundQueued,
		eventlog.PartitionWebhook("ep-1"),
		eventlog.WebhookOutboundQueuedPayload{
			DeliveryID:  "d-1",
			EndpointID:  "ep-1",
			URL:         "https://example.com/x",
			Method:      "POST",
			Body:        `{"k":"v"}`,
			MaxAttempts: 3,
		})
	if err := s.Apply(context.Background(), nil, env); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	pending := s.PendingForTest()
	if pending["d-1"] != 1 {
		t.Fatalf("want d-1 attempt=1 in heap, got %+v", pending)
	}
}

func TestWebhookOutboundSender_ApplyScheduledRecoversFromQueued(t *testing.T) {
	log := eventlogtest.NewMemoryLog()
	s := NewWebhookOutboundSender(log, Options{})

	queued := envFromPayload(t, eventlog.EventTypeWebhookOutboundQueued,
		eventlog.PartitionWebhook("ep-2"),
		eventlog.WebhookOutboundQueuedPayload{
			DeliveryID:  "d-2",
			EndpointID:  "ep-2",
			URL:         "https://example.com/y",
			Method:      "POST",
			Body:        `body`,
			MaxAttempts: 5,
		})
	log.Append(t, eventlog.EnvelopeDraft{
		Partition: queued.Partition,
		Type:      queued.Type,
		Version:   1,
		Category:  "business",
		Payload: eventlog.WebhookOutboundQueuedPayload{
			DeliveryID:  "d-2",
			EndpointID:  "ep-2",
			URL:         "https://example.com/y",
			Method:      "POST",
			Body:        `body`,
			MaxAttempts: 5,
		},
	})

	scheduled := envFromPayload(t, eventlog.EventTypeWebhookOutboundScheduled,
		eventlog.PartitionWebhook("ep-2"),
		eventlog.WebhookOutboundScheduledPayload{
			DeliveryID: "d-2",
			EndpointID: "ep-2",
			Attempt:    3,
			NotBefore:  time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		})
	if err := s.Apply(context.Background(), nil, scheduled); err != nil {
		t.Fatalf("Apply scheduled: %v", err)
	}
	pending := s.PendingForTest()
	if pending["d-2"] != 3 {
		t.Fatalf("expected d-2 attempt=3 after recovery, got %+v", pending)
	}
}
