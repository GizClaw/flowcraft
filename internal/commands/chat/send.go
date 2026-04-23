// Package chat contains command handlers for chat events.
package chat

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/policy"
)

// ChatSendReq is the request for ChatSendCommand.
type ChatSendReq struct {
	ConversationID string
	CardID         string
	Role           string
	Content        string
	// TokenCount is optional; only populated when the producer (e.g. the
	// agent runtime) knows the LLM-reported token usage for the message.
	TokenCount int64
	CommandID  string
}

// ChatSendCommand publishes chat.message.sent events.
type ChatSendCommand struct {
	log eventlog.Log
}

// NewChatSendCommand constructs a ChatSendCommand.
func NewChatSendCommand(log eventlog.Log) *ChatSendCommand {
	return &ChatSendCommand{log: log}
}

// Handle publishes a chat.message.sent event idempotently.
// Actor is read from ctx; client-provided user_id is never trusted (D.15).
func (c *ChatSendCommand) Handle(ctx context.Context, req ChatSendReq) (messageID string, err error) {
	if req.CommandID == "" {
		req.CommandID = newCommandID()
	}
	if req.CardID == "" {
		req.CardID = req.ConversationID
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
		return eventlog.PublishChatMessageSentInTx(ctx, uow, req.CardID, eventlog.ChatMessageSentPayload{
			CardID:         req.CardID,
			ConversationID: req.ConversationID,
			MessageID:      req.CommandID,
			Role:           req.Role,
			Content:        req.Content,
			TokenCount:     req.TokenCount,
		}, eventlog.WithActor(actor.ToWire()))
	})
	return req.CommandID, err
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
