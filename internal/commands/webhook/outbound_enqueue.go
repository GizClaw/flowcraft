package webhook

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/policy"
)

// OutboundEnqueueReq is the request for WebhookOutboundEnqueueCommand.
type OutboundEnqueueReq struct {
	EndpointID       string
	URL              string
	Method           string
	Headers          map[string]string
	Body             string
	MaxAttempts      int32 // default 5
	InitialBackoffMs int32 // default 1000
	SourceEventSeq   int64
	CommandID        string // used both as command_dedup id and delivery_id
}

// WebhookOutboundEnqueueCommand publishes webhook.outbound.queued events that
// the WebhookOutboundSender consumes.
type WebhookOutboundEnqueueCommand struct {
	log eventlog.Log
}

// NewWebhookOutboundEnqueueCommand constructs a WebhookOutboundEnqueueCommand.
func NewWebhookOutboundEnqueueCommand(log eventlog.Log) *WebhookOutboundEnqueueCommand {
	return &WebhookOutboundEnqueueCommand{log: log}
}

// Handle publishes a webhook.outbound.queued event idempotently. The full
// HTTP request descriptor is captured in the envelope payload so that any
// process restart can rebuild the in-memory delivery heap from event_log.
func (c *WebhookOutboundEnqueueCommand) Handle(ctx context.Context, req OutboundEnqueueReq) (deliveryID string, err error) {
	if req.CommandID == "" {
		req.CommandID = newCommandID()
	}
	if req.MaxAttempts == 0 {
		req.MaxAttempts = 5
	}
	if req.InitialBackoffMs == 0 {
		req.InitialBackoffMs = 1000
	}
	if req.Method == "" {
		req.Method = "POST"
	}
	actor, _ := policy.ActorFrom(ctx)
	now := time.Now().UTC()
	_, err = c.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		if dup, err := isDuplicateCommand(ctx, uow, req.CommandID); err != nil {
			return err
		} else if dup {
			return nil
		}
		if _, err := uow.BusinessExec(ctx,
			`INSERT INTO command_dedup(command_id, executed_at) VALUES(?, ?)`,
			req.CommandID, now.Format(time.RFC3339Nano)); err != nil {
			return err
		}
		return eventlog.PublishWebhookOutboundQueuedInTx(ctx, uow, req.EndpointID, eventlog.WebhookOutboundQueuedPayload{
			DeliveryID:       req.CommandID,
			EndpointID:       req.EndpointID,
			URL:              req.URL,
			Method:           req.Method,
			Headers:          req.Headers,
			Body:             req.Body,
			MaxAttempts:      req.MaxAttempts,
			InitialBackoffMs: req.InitialBackoffMs,
			SourceEventSeq:   req.SourceEventSeq,
		}, eventlog.WithActor(actor.ToWire()))
	})
	return req.CommandID, err
}
