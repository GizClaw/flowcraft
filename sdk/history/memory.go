// Package history manages conversation transcripts: append messages,
// load them back fitted to a model's context window, and (optionally)
// compact older turns into hierarchical summaries to keep that window
// finite. Long-term fact recall lives in [sdk/recall].
//
// Migration note: this package was renamed from sdk/memory in v0.2.0.
// The Save/Load/Clear interface and the LosslessMemory/BufferMemory/
// SummaryDAG/Archive types are unchanged in this commit; later commits
// in the same series tighten the contract (Save → Append) and add
// atomic-write durability.
package history

import (
	"context"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// Memory is the strategy-layer interface that decides which messages to return to the LLM.
type Memory interface {
	Load(ctx context.Context, conversationID string) ([]model.Message, error)
	Save(ctx context.Context, conversationID string, messages []model.Message) error
	Clear(ctx context.Context, conversationID string) error
}

// Store is the persistence-layer interface for short-term message storage.
type Store interface {
	GetMessages(ctx context.Context, conversationID string) ([]model.Message, error)
	SaveMessages(ctx context.Context, conversationID string, messages []model.Message) error
	DeleteMessages(ctx context.Context, conversationID string) error
}

// RecentReader is an optional interface for stores that can efficiently
// read only the most recent N messages for a conversation.
type RecentReader interface {
	GetRecentMessages(ctx context.Context, conversationID string, limit int) ([]model.Message, error)
}

// MessageAppender is an optional interface for stores that can append
// only the newly generated messages without reloading the full history.
type MessageAppender interface {
	AppendMessages(ctx context.Context, conversationID string, messages []model.Message) error
}

// RangeReader is an optional interface for stores that support
// reading a subset of messages by sequence range.
type RangeReader interface {
	GetMessageRange(ctx context.Context, conversationID string, start, end int) ([]model.Message, error)
}

// SummaryCacheStore is an optional interface that Store implementations
// can satisfy to persist summary caches alongside messages.
type SummaryCacheStore interface {
	GetSummary(ctx context.Context, conversationID string) (summary string, msgCount int, err error)
	SaveSummary(ctx context.Context, conversationID, summary string, msgCount int) error
}

// Config configures the conversation memory. All memory is lossless by default;
// the Type field is deprecated and ignored (kept for backward compatibility).
type Config struct {
	Type        string         `json:"type,omitempty"` // deprecated: ignored, always lossless
	MaxMessages int            `json:"max_messages,omitempty"`
	LongTerm    recall.LongTermConfig `json:"long_term,omitempty"`
	Lossless    LosslessConfig `json:"lossless,omitempty"`
}

// LosslessConfig controls the lossless DAG memory behavior.
type LosslessConfig struct {
	ChunkSize         int     `json:"chunk_size,omitempty"`
	CondenseThreshold int     `json:"condense_threshold,omitempty"`
	MaxDepth          int     `json:"max_depth,omitempty"`
	TokenBudget       int     `json:"token_budget,omitempty"`
	RecentRatio       float64 `json:"recent_ratio,omitempty"`
	CompactThreshold  int     `json:"compact_threshold,omitempty"`
	PruneLeafContent  *bool   `json:"prune_leaf_content,omitempty"`
	ArchiveThreshold  int     `json:"archive_threshold,omitempty"`
	ArchiveBatchSize  int     `json:"archive_batch_size,omitempty"`
}

func (c LosslessConfig) toDAGConfig() DAGConfig {
	cfg := DefaultDAGConfig()
	if c.ChunkSize > 0 {
		cfg.ChunkSize = c.ChunkSize
	}
	if c.CondenseThreshold > 0 {
		cfg.CondenseThreshold = c.CondenseThreshold
	}
	if c.MaxDepth > 0 {
		cfg.MaxDepth = c.MaxDepth
	}
	if c.TokenBudget > 0 {
		cfg.TokenBudget = c.TokenBudget
	}
	if c.RecentRatio > 0 {
		cfg.RecentRatio = c.RecentRatio
	}
	if c.CompactThreshold > 0 {
		cfg.Compact.CompactThreshold = c.CompactThreshold
	}
	if c.PruneLeafContent != nil {
		cfg.Compact.PruneLeafContent = *c.PruneLeafContent
	}
	if c.ArchiveThreshold > 0 {
		cfg.Archive.ArchiveThreshold = c.ArchiveThreshold
	}
	if c.ArchiveBatchSize > 0 {
		cfg.Archive.ArchiveBatchSize = c.ArchiveBatchSize
	}
	return cfg
}

const (
	defaultMaxConversations = 10000
	defaultConversationTTL  = 24 * time.Hour
)

// InMemoryStoreOption configures an InMemoryStore.
type InMemoryStoreOption func(*InMemoryStore)

// WithMaxConversations sets the upper bound on stored conversations.
// When exceeded, the least-recently-accessed conversation is evicted.
func WithMaxConversations(n int) InMemoryStoreOption {
	return func(s *InMemoryStore) {
		if n > 0 {
			s.maxConversations = n
		}
	}
}

// WithTTL sets how long an idle conversation is kept before eviction.
func WithTTL(d time.Duration) InMemoryStoreOption {
	return func(s *InMemoryStore) {
		if d > 0 {
			s.ttl = d
		}
	}
}

type memEntry struct {
	messages   []model.Message
	lastAccess time.Time
}

// InMemoryStore is a simple in-memory message store for development and testing.
// It supports a maximum conversation count and TTL-based eviction.
type InMemoryStore struct {
	mu               sync.RWMutex
	data             map[string]*memEntry
	maxConversations int
	ttl              time.Duration
	done             chan struct{}
}

// NewInMemoryStore creates a new in-memory message store with optional limits.
func NewInMemoryStore(opts ...InMemoryStoreOption) *InMemoryStore {
	s := &InMemoryStore{
		data:             make(map[string]*memEntry),
		maxConversations: defaultMaxConversations,
		ttl:              defaultConversationTTL,
		done:             make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	go s.cleanupLoop()
	return s
}

// Close stops the background cleanup goroutine.
func (s *InMemoryStore) Close() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

func (s *InMemoryStore) cleanupLoop() {
	ticker := time.NewTicker(s.ttl / 2)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.evictExpired()
		}
	}
}

func (s *InMemoryStore) evictExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-s.ttl)
	for id, entry := range s.data {
		if entry.lastAccess.Before(cutoff) {
			delete(s.data, id)
		}
	}
}

// evictOldest removes the least-recently-accessed entry. Caller must hold s.mu.
func (s *InMemoryStore) evictOldest() {
	var oldestID string
	var oldestTime time.Time
	for id, entry := range s.data {
		if oldestID == "" || entry.lastAccess.Before(oldestTime) {
			oldestID = id
			oldestTime = entry.lastAccess
		}
	}
	if oldestID != "" {
		delete(s.data, oldestID)
	}
}

func (s *InMemoryStore) GetMessages(_ context.Context, conversationID string) ([]model.Message, error) {
	s.mu.Lock()
	entry := s.data[conversationID]
	if entry == nil {
		s.mu.Unlock()
		return nil, nil
	}
	entry.lastAccess = time.Now()
	cp := make([]model.Message, len(entry.messages))
	copy(cp, entry.messages)
	s.mu.Unlock()
	return cp, nil
}

func (s *InMemoryStore) SaveMessages(_ context.Context, conversationID string, messages []model.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]model.Message, len(messages))
	copy(cp, messages)
	if _, exists := s.data[conversationID]; !exists && len(s.data) >= s.maxConversations {
		s.evictOldest()
	}
	s.data[conversationID] = &memEntry{messages: cp, lastAccess: time.Now()}
	return nil
}

func (s *InMemoryStore) DeleteMessages(_ context.Context, conversationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, conversationID)
	return nil
}

// Len returns the number of stored conversations (useful for testing/monitoring).
func (s *InMemoryStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}
