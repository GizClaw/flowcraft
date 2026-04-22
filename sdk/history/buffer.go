package history

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// BufferMemory keeps the last N messages.
type BufferMemory struct {
	store       Store
	maxMessages int
}

// NewBufferMemory creates a buffer memory with a maximum message count.
func NewBufferMemory(store Store, maxMessages int) *BufferMemory {
	if maxMessages <= 0 {
		maxMessages = 50
	}
	return &BufferMemory{store: store, maxMessages: maxMessages}
}

func (m *BufferMemory) Load(ctx context.Context, conversationID string) ([]model.Message, error) {
	if recent, ok := m.store.(RecentReader); ok {
		return recent.GetRecentMessages(ctx, conversationID, m.maxMessages)
	}
	msgs, err := m.store.GetMessages(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	if len(msgs) > m.maxMessages {
		msgs = msgs[len(msgs)-m.maxMessages:]
	}
	return msgs, nil
}

func (m *BufferMemory) Save(ctx context.Context, conversationID string, messages []model.Message) error {
	return m.store.SaveMessages(ctx, conversationID, messages)
}

func (m *BufferMemory) Clear(ctx context.Context, conversationID string) error {
	return m.store.DeleteMessages(ctx, conversationID)
}
