package history

import (
	"context"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// buffer is the tail-truncation implementation of [History]. It is
// unexported on purpose: callers construct it via [NewBuffer] and
// consume it through [History].
type buffer struct {
	store       Store
	maxMessages int

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// BufferOption customizes a [History] built by [NewBuffer].
type BufferOption func(*buffer)

// WithBufferMax sets the maximum message count kept in the returned
// [History]. Must be > 0; values ≤ 0 are ignored and the default (50) is
// kept.
func WithBufferMax(n int) BufferOption {
	return func(b *buffer) {
		if n > 0 {
			b.maxMessages = n
		}
	}
}

// NewBuffer returns a [History] that keeps the most recent messages for
// each conversation up to a cap (default 50; override with
// [WithBufferMax]).
//
// It is the simplest History implementation — appends concatenate, loads
// truncate. Use it for short sessions, tests, and examples; switch to
// [NewCompacted] when a conversation needs to outgrow a single context
// window.
func NewBuffer(store Store, opts ...BufferOption) History {
	b := &buffer{
		store:       store,
		maxMessages: 50,
		locks:       make(map[string]*sync.Mutex),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

func (m *buffer) Load(ctx context.Context, conversationID string, budget Budget) ([]model.Message, error) {
	limit := m.maxMessages
	if budget.MaxMessages > 0 && budget.MaxMessages < limit {
		limit = budget.MaxMessages
	}
	if recent, ok := m.store.(RecentReader); ok {
		return recent.GetRecentMessages(ctx, conversationID, limit)
	}
	msgs, err := m.store.GetMessages(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	if len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	return msgs, nil
}

// Append persists newMessages, serializing concurrent calls per conversation
// so a read-modify-write fallback path can never lose data.
func (m *buffer) Append(ctx context.Context, conversationID string, newMessages []model.Message) error {
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

func (m *buffer) Clear(ctx context.Context, conversationID string) error {
	mu := m.convMu(conversationID)
	mu.Lock()
	defer mu.Unlock()
	return m.store.DeleteMessages(ctx, conversationID)
}

func (m *buffer) convMu(convID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	mu, ok := m.locks[convID]
	if !ok {
		mu = &sync.Mutex{}
		m.locks[convID] = mu
	}
	return mu
}
