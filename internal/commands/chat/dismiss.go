package chat

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/policy"
)

// ChatDismissReq is the request for ChatDismissCommand.
type ChatDismissReq struct {
	ConversationID string
	CardID         string
	CallbackID     string
	Reason         string
	CommandID      string
}

// ChatDismissCommand publishes chat.callback.dismissed events.
type ChatDismissCommand struct {
	log eventlog.Log
}

// NewChatDismissCommand constructs a ChatDismissCommand.
func NewChatDismissCommand(log eventlog.Log) *ChatDismissCommand {
	return &ChatDismissCommand{log: log}
}

// Handle publishes a chat.callback.dismissed event idempotently.
func (c *ChatDismissCommand) Handle(ctx context.Context, req ChatDismissReq) error {
	if req.CommandID == "" {
		req.CommandID = newCommandID()
	}
	if req.CardID == "" {
		req.CardID = req.ConversationID
	}
	actor, _ := policy.ActorFrom(ctx)
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
		return eventlog.PublishChatCallbackDismissedInTx(ctx, uow, req.CardID, eventlog.ChatCallbackDismissedPayload{
			CardID:         req.CardID,
			ConversationID: req.ConversationID,
			CallbackID:     req.CallbackID,
			Reason:         req.Reason,
		}, eventlog.WithActor(actorOrAnonymous(actor)))
	})
	return err
}
