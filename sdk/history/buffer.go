package history

import (
	"context"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// BufferMemory keeps the last N messages.
type BufferMemory struct {
	store       Store
	maxMessages int

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewBufferMemory creates a buffer memory with a maximum message count.
func NewBufferMemory(store Store, maxMessages int) *BufferMemory {
	if maxMessages <= 0 {
		maxMessages = 50
	}
	return &BufferMemory{
		store:       store,
		maxMessages: maxMessages,
		locks:       make(map[string]*sync.Mutex),
	}
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

// Append persists newMessages, serializing concurrent calls per conversation
// so a read-modify-write fallback path can never lose data.
func (m *BufferMemory) Append(ctx context.Context, conversationID string, newMessages []model.Message) error {
	if len(newMessages) == 0 {
		return nil
	}
	mu := m.convMu(conversationID)
	mu.Lock()
	defer mu.Unlock()

	if appender, ok := m.store.(MessageAppender); ok {
		return appender.AppendMessages(ctx, conversationID, newMessages)
	}

	// Fallback: read existing + concat + rewrite. Safe because the
	// per-conversation lock above guarantees no concurrent writer.
	existing, err := m.store.GetMessages(ctx, conversationID)
	if err != nil {
		return err
	}
	combined := make([]model.Message, 0, len(existing)+len(newMessages))
	combined = append(combined, existing...)
	combined = append(combined, newMessages...)
	return m.store.SaveMessages(ctx, conversationID, combined)
}

func (m *BufferMemory) Clear(ctx context.Context, conversationID string) error {
	mu := m.convMu(conversationID)
	mu.Lock()
	defer mu.Unlock()
	return m.store.DeleteMessages(ctx, conversationID)
}

func (m *BufferMemory) convMu(convID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	mu, ok := m.locks[convID]
	if !ok {
		mu = &sync.Mutex{}
		m.locks[convID] = mu
	}
	return mu
}
