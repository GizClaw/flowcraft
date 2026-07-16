package claw

import (
	"context"
	"fmt"
	"strings"

	memoryhistory "github.com/GizClaw/flowcraft/memory/history"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// HistoryConfig controls short-term transcript history keyed by context_id.
type HistoryConfig struct {
	Enabled     bool   `json:"enabled,omitempty"`
	Kind        string `json:"kind,omitempty"`
	MaxMessages int    `json:"max_messages,omitempty"`
	MaxTokens   int    `json:"max_tokens,omitempty"`
}

type historyRuntime struct {
	hist memoryhistory.History
	cfg  HistoryConfig
}

func (c *Claw) buildHistory(_ context.Context) (*historyRuntime, error) {
	cfg := c.cfg.History
	if !cfg.Enabled {
		return nil, nil
	}
	switch strings.TrimSpace(cfg.Kind) {
	case "", "buffer":
		store := c.historyStore
		if store == nil {
			store = memoryhistory.NewFileStore(c.ws, c.cfg.Workspace.HistoryRoot)
		}
		opts := []memoryhistory.BufferOption{}
		if cfg.MaxMessages > 0 {
			opts = append(opts, memoryhistory.WithBufferMax(cfg.MaxMessages))
		}
		return &historyRuntime{
			hist: memoryhistory.NewBuffer(store, opts...),
			cfg:  cfg,
		}, nil
	case "compacted":
		return nil, fmt.Errorf("claw: history.kind=%q is not wired yet", cfg.Kind)
	default:
		return nil, fmt.Errorf("claw: history.kind=%q is invalid", cfg.Kind)
	}
}

func (h *historyRuntime) load(ctx context.Context, contextID string) ([]model.Message, error) {
	if h == nil || h.hist == nil || strings.TrimSpace(contextID) == "" {
		return nil, nil
	}
	return h.hist.Load(ctx, contextID, memoryhistory.Budget{
		MaxMessages: h.cfg.MaxMessages,
		MaxTokens:   h.cfg.MaxTokens,
	})
}

func (h *historyRuntime) appendTurn(ctx context.Context, contextID string, user model.Message, assistant []model.Message) error {
	if h == nil || h.hist == nil || strings.TrimSpace(contextID) == "" {
		return nil
	}
	messages := make([]model.Message, 0, 1+len(assistant))
	if user.Role != "" || len(user.Parts) > 0 {
		messages = append(messages, user)
	}
	messages = append(messages, assistant...)
	return h.hist.Append(ctx, contextID, messages)
}
