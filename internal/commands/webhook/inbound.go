// Package webhook contains command handlers for webhook events.
package webhook

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/policy"
)

// InboundReq is the request for WebhookInboundCommand.
type InboundReq struct {
	EndpointID    string
	IdemKey       string // X-Idempotency-Key header value (optional)
	HTTPMethod    string
	Headers       map[string]string
	Body          string
	ContentType   string
	RemoteAddr    string
	SignatureKind string
	CommandID     string
}

// WebhookInboundCommand publishes webhook.inbound.received events.
type WebhookInboundCommand struct {
	log eventlog.Log
}

// NewWebhookInboundCommand constructs a WebhookInboundCommand.
func NewWebhookInboundCommand(log eventlog.Log) *WebhookInboundCommand {
	return &WebhookInboundCommand{log: log}
}

// Handle persists the inbound webhook idempotently. If the IdemKey is already
// in webhook_inbound_idem, returns the previous received_id and alreadyProcessed=true.
func (c *WebhookInboundCommand) Handle(ctx context.Context, req InboundReq) (receivedID string, alreadyProcessed bool, err error) {
	if req.CommandID == "" {
		req.CommandID = newCommandID()
	}
	actor, _ := policy.ActorFrom(ctx)
	now := time.Now().UTC()
	_, err = c.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		if req.IdemKey != "" {
			var prev string
			row := uow.BusinessQueryRow(ctx,
				`SELECT received_id FROM webhook_inbound_idem WHERE endpoint_id=? AND idem_key=?`,
				req.EndpointID, req.IdemKey)
			if err := row.Scan(&prev); err == nil {
				receivedID = prev
				alreadyProcessed = true
				return nil
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if _, err := uow.BusinessExec(ctx,
				`INSERT INTO webhook_inbound_idem(endpoint_id, idem_key, received_id, seq, ts) VALUES(?, ?, ?, ?, ?)`,
				req.EndpointID, req.IdemKey, req.CommandID, 0, now.Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
		receivedID = req.CommandID
		return eventlog.PublishWebhookInboundReceivedInTx(ctx, uow, req.EndpointID, eventlog.WebhookInboundBody{
			EndpointID:  req.EndpointID,
			ContentType: req.ContentType,
			Headers:     filterHeaders(req.Headers),
			Body:        req.Body,
		}, eventlog.WithActor(actor.ToWire()))
	})
	return receivedID, alreadyProcessed, err
}

func filterHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	sensitive := map[string]bool{
		"authorization": true,
		"cookie":        true,
		"set-cookie":    true,
		"x-api-key":     true,
	}
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		if sensitive[strings.ToLower(k)] {
			continue
		}
		out[k] = v
	}
	return out
}

func newCommandID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func isDuplicateCommand(ctx context.Context, uow eventlog.UnitOfWork, commandID string) (bool, error) {
	var dummy int
	row := uow.BusinessQueryRow(ctx, `SELECT 1 FROM command_dedup WHERE command_id=?`, commandID)
	if err := row.Scan(&dummy); err == nil {
		return true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	return false, nil
}

