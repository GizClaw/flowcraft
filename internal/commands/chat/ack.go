package chat

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/policy"
)

// ChatAckReq is the request for ChatAckCommand.
type ChatAckReq struct {
	ConversationID string
	CardID         string
	CallbackID     string
	CommandID      string
}

// ChatAckCommand publishes chat.callback.delivered events.
type ChatAckCommand struct {
	log eventlog.Log
}

// NewChatAckCommand constructs a ChatAckCommand.
func NewChatAckCommand(log eventlog.Log) *ChatAckCommand {
	return &ChatAckCommand{log: log}
}

// Handle publishes a chat.callback.delivered event idempotently. The actor
// must come from ctx (D.15: client-supplied user_id is never trusted).
func (c *ChatAckCommand) Handle(ctx context.Context, req ChatAckReq) error {
	if req.CommandID == "" {
		req.CommandID = newCommandID()
	}
	if req.CardID == "" {
		req.CardID = req.ConversationID
	}
	actor, _ := policy.ActorFrom(ctx)
	deliveredTo := "user_ack"
	if actor.Type == policy.ActorSystem {
		deliveredTo = "system_auto"
	}
	now := time.Now().UTC()
	_, err := c.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
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
		return eventlog.PublishChatCallbackDeliveredInTx(ctx, uow, req.CardID, eventlog.ChatCallbackDeliveredPayload{
			CardID:         req.CardID,
			ConversationID: req.ConversationID,
			CallbackID:     req.CallbackID,
			DeliveredTo:    deliveredTo,
			Success:        true,
		}, eventlog.WithActor(actor.ToWire()))
	})
	return err
}
