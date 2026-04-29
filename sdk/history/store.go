package history

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/workspace"

	otellog "go.opentelemetry.io/otel/log"
)

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
// can satisfy to persist a single "current summary" string alongside
// messages.
//
// Deprecated: superseded by [SummaryStore], which the [SummaryDAG]
// already requires. Nothing inside the history package consumes this
// interface any more — the [InMemoryStore] / [FileStore] implementations
// remain only so downstream Stores that already satisfied it keep
// compiling. Will be removed in v0.3.0.
//
// # Migration
//
// Most callers wired SummaryCacheStore in v0.1 to enable a hand-rolled
// "buffer + single summary" history strategy. The supported v0.2+
// equivalent is [NewCompacted], which manages a multi-level summary DAG
// and assembles "highest-depth summary + recent tail" into the supplied
// token budget automatically — no SummaryCacheStore plumbing required:
//
//	hist := history.NewCompacted(store, summaryLLM, ws,
//	    history.WithTokenBudget(8000),
//	)
//	msgs, _ := hist.Load(ctx, convID, history.Budget{})
//
// If the single-summary semantics are still desired (for example
// because the downstream service has its own summarizer and only needs
// a place to persist the result), the same shape can be expressed in
// ~5 lines on top of [SummaryStore]:
//
//	// Save (overwrite the single summary):
//	_ = ss.Rewrite(ctx, convID, []*history.SummaryNode{{
//	    ID:             history.NewSummaryNodeID(),
//	    ConversationID: convID,
//	    Depth:          0,
//	    Content:        text,
//	    EarliestSeq:    0,
//	    LatestSeq:      msgCount - 1,
//	    CreatedAt:      time.Now(),
//	}})
//
//	// Load:
//	depth0 := 0
//	nodes, _ := ss.List(ctx, convID, history.SummaryListOptions{Depth: &depth0})
//	// nodes[0].Content is the summary; nodes[0].LatestSeq+1 is msgCount.
//
// [SummaryStore.Rewrite] is the exact primitive SummaryCacheStore.SaveSummary
// implied (atomic single-record replacement); the only thing v0.3 drops is
// the dedicated interface name.
type SummaryCacheStore interface {
	GetSummary(ctx context.Context, conversationID string) (summary string, msgCount int, err error)
	SaveSummary(ctx context.Context, conversationID, summary string, msgCount int) error
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

// FileStore is a Workspace-backed Store that persists messages as JSONL files.
// Each conversation is stored at {prefix}/{conversationID}/messages.jsonl with
// one JSON-encoded model.Message per line. Saves are incremental.
type FileStore struct {
	ws     workspace.Workspace
	prefix string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewFileStore creates a FileStore rooted at the given prefix directory
// within the workspace (e.g. "memory").
func NewFileStore(ws workspace.Workspace, prefix string) *FileStore {
	return &FileStore{
		ws:     ws,
		prefix: prefix,
		locks:  make(map[string]*sync.Mutex),
	}
}

func (s *FileStore) convMu(conversationID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.locks[conversationID]
	if !ok {
		m = &sync.Mutex{}
		s.locks[conversationID] = m
	}
	return m
}

func (s *FileStore) messagesPath(conversationID string) string {
	return fmt.Sprintf("%s/%s/messages.jsonl", s.prefix, conversationID)
}

func (s *FileStore) GetMessages(ctx context.Context, conversationID string) ([]model.Message, error) {
	mu := s.convMu(conversationID)
	mu.Lock()
	defer mu.Unlock()
	return s.readMessages(ctx, conversationID)
}

func (s *FileStore) readMessages(ctx context.Context, conversationID string) ([]model.Message, error) {
	path := s.messagesPath(conversationID)
	exists, err := s.ws.Exists(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("memory: check exists %q: %w", path, err)
	}
	if !exists {
		return nil, nil
	}

	data, err := s.ws.Read(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("memory: read %q: %w", path, err)
	}

	var msgs []model.Message
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var msg model.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			telemetry.Warn(ctx, "memory: skipping malformed line",
				otellog.String("path", path),
				otellog.Int("line", lineNum),
				otellog.String(telemetry.AttrErrorMessage, err.Error()))
			continue
		}
		msgs = append(msgs, msg)
	}
	if err := scanner.Err(); err != nil {
		return msgs, fmt.Errorf("memory: scan %q: %w", path, err)
	}
	return msgs, nil
}

func (s *FileStore) SaveMessages(ctx context.Context, conversationID string, messages []model.Message) error {
	mu := s.convMu(conversationID)
	mu.Lock()
	defer mu.Unlock()

	existing, err := s.readMessages(ctx, conversationID)
	if err != nil {
		return err
	}

	path := s.messagesPath(conversationID)

	if len(existing) == 0 {
		return s.writeAll(ctx, path, messages)
	}
	if len(messages) <= len(existing) {
		if len(messages) < len(existing) {
			return s.writeAll(ctx, path, messages)
		}
		return nil
	}

	newMsgs := messages[len(existing):]
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, msg := range newMsgs {
		if err := enc.Encode(msg); err != nil {
			return fmt.Errorf("memory: encode message: %w", err)
		}
	}
	if err := s.ws.Append(ctx, path, buf.Bytes()); err != nil {
		return fmt.Errorf("memory: append %q: %w", path, err)
	}
	return nil
}

func (s *FileStore) DeleteMessages(ctx context.Context, conversationID string) error {
	mu := s.convMu(conversationID)
	mu.Lock()

	dir := fmt.Sprintf("%s/%s", s.prefix, conversationID)
	if err := s.ws.RemoveAll(ctx, dir); err != nil {
		mu.Unlock()
		return fmt.Errorf("memory: delete conversation %q: %w", conversationID, err)
	}

	// Remove the lock entry while still holding it, so no concurrent caller
	// can observe a deleted-but-unlocked mutex. New callers will create a
	// fresh mutex via convMu().
	s.mu.Lock()
	delete(s.locks, conversationID)
	s.mu.Unlock()
	mu.Unlock()

	return nil
}

// --- SummaryCacheStore implementation ---

func (s *FileStore) summaryPath(conversationID string) string {
	return fmt.Sprintf("%s/%s/summary.json", s.prefix, conversationID)
}

type summaryJSON struct {
	Text     string `json:"text"`
	MsgCount int    `json:"msg_count"`
}

func (s *FileStore) GetSummary(ctx context.Context, conversationID string) (string, int, error) {
	mu := s.convMu(conversationID)
	mu.Lock()
	defer mu.Unlock()

	path := s.summaryPath(conversationID)
	exists, err := s.ws.Exists(ctx, path)
	if err != nil || !exists {
		return "", 0, err
	}
	data, err := s.ws.Read(ctx, path)
	if err != nil {
		return "", 0, fmt.Errorf("memory: read summary cache: %w", err)
	}
	var cache summaryJSON
	if err := json.Unmarshal(data, &cache); err != nil {
		return "", 0, fmt.Errorf("memory: unmarshal summary cache: %w", err)
	}
	return cache.Text, cache.MsgCount, nil
}

func (s *FileStore) SaveSummary(ctx context.Context, conversationID, summary string, msgCount int) error {
	mu := s.convMu(conversationID)
	mu.Lock()
	defer mu.Unlock()

	data, err := json.Marshal(summaryJSON{Text: summary, MsgCount: msgCount})
	if err != nil {
		return fmt.Errorf("memory: marshal summary cache: %w", err)
	}
	path := s.summaryPath(conversationID)
	if err := s.ws.Write(ctx, path, data); err != nil {
		return fmt.Errorf("memory: write summary cache: %w", err)
	}
	return nil
}

// GetMessageRange returns messages in the range [start, end).
func (s *FileStore) GetMessageRange(ctx context.Context, conversationID string, start, end int) ([]model.Message, error) {
	mu := s.convMu(conversationID)
	mu.Lock()
	defer mu.Unlock()

	msgs, err := s.readMessages(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	if start < 0 {
		start = 0
	}
	if end > len(msgs) {
		end = len(msgs)
	}
	if start >= end {
		return nil, nil
	}
	result := make([]model.Message, end-start)
	copy(result, msgs[start:end])
	return result, nil
}

func (s *FileStore) writeAll(ctx context.Context, path string, messages []model.Message) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, msg := range messages {
		if err := enc.Encode(msg); err != nil {
			return fmt.Errorf("memory: encode message: %w", err)
		}
	}
	if err := s.ws.Write(ctx, path, buf.Bytes()); err != nil {
		return fmt.Errorf("memory: write %q: %w", path, err)
	}
	return nil
}
