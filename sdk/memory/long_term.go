package memory

import (
	"context"
	"sync"
	"time"
)

// MemoryCategory classifies a long-term memory entry.
type MemoryCategory string

const (
	CategoryProfile     MemoryCategory = "profile"
	CategoryPreferences MemoryCategory = "preferences"
	CategoryEntities    MemoryCategory = "entities"
	CategoryEvents      MemoryCategory = "events"
	CategoryCases       MemoryCategory = "cases"
	CategoryPatterns    MemoryCategory = "patterns"
)

var defaultCategories = []MemoryCategory{
	CategoryProfile, CategoryPreferences, CategoryEntities,
	CategoryEvents, CategoryCases, CategoryPatterns,
}

var categoryRegistry struct {
	mu         sync.RWMutex
	registered []MemoryCategory
}

// RegisterCategory adds a custom memory category to the global registry.
// Must be called before pipeline usage (typically in init or startup).
func RegisterCategory(cat MemoryCategory) {
	categoryRegistry.mu.Lock()
	defer categoryRegistry.mu.Unlock()
	for _, c := range categoryRegistry.registered {
		if c == cat {
			return
		}
	}
	categoryRegistry.registered = append(categoryRegistry.registered, cat)
}

// AllCategories returns all built-in and registered memory categories.
func AllCategories() []MemoryCategory {
	categoryRegistry.mu.RLock()
	extra := categoryRegistry.registered
	categoryRegistry.mu.RUnlock()

	if len(extra) == 0 {
		return append([]MemoryCategory(nil), defaultCategories...)
	}
	all := make([]MemoryCategory, 0, len(defaultCategories)+len(extra))
	all = append(all, defaultCategories...)
	all = append(all, extra...)
	return all
}

// AllCategoryStrings returns all category names as strings.
func AllCategoryStrings() []string {
	cats := AllCategories()
	out := make([]string, len(cats))
	for i, c := range cats {
		out[i] = string(c)
	}
	return out
}

// MemoryScope partitions long-term memory within a runtime (e.g. per end user).
// Empty UserID and SessionID denotes the runtime-global bucket (shared memories).
type MemoryScope struct {
	RuntimeID string `json:"runtime_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// IsGlobal returns true when this scope refers only to the runtime-wide bucket.
func (s MemoryScope) IsGlobal() bool {
	return s.UserID == "" && s.SessionID == ""
}

// CacheKey returns a stable key for assembler caches (must include runtime + user/session).
func (s MemoryScope) CacheKey() string {
	if s.SessionID != "" {
		return s.RuntimeID + "|" + s.UserID + "|" + s.SessionID
	}
	if s.UserID != "" {
		return s.RuntimeID + "|" + s.UserID
	}
	return s.RuntimeID
}

// MemoryEntry is a single long-term memory record.
type MemoryEntry struct {
	ID        string         `json:"id"`
	Category  MemoryCategory `json:"category"`
	Content   string         `json:"content"`
	Keywords  []string       `json:"keywords,omitempty"`
	Source    MemorySource   `json:"source"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	Scope     MemoryScope    `json:"scope,omitempty"`
}

// MemorySource records the origin of a memory entry.
type MemorySource struct {
	RuntimeID      string    `json:"runtime_id,omitempty"`
	ConversationID string    `json:"conversation_id,omitempty"`
	Timestamp      time.Time `json:"timestamp,omitempty"`
}

// LongTermStore is the persistence interface for runtime-scoped long-term memory.
type LongTermStore interface {
	Save(ctx context.Context, runtimeID string, entry *MemoryEntry) error
	List(ctx context.Context, runtimeID string, opts ListOptions) ([]*MemoryEntry, error)
	Search(ctx context.Context, runtimeID string, query string, opts SearchOptions) ([]*MemoryEntry, error)
	Update(ctx context.Context, runtimeID string, entry *MemoryEntry) error
	Delete(ctx context.Context, runtimeID, entryID string) error
}

// ListOptions configures listing of long-term memory entries.
type ListOptions struct {
	Category MemoryCategory
	Limit    int
	// Scope, when non-nil, restricts rows to entries in that scope bucket.
	// nil preserves legacy behavior (no scope filter).
	// If Recall is set, Scope is ignored for filtering (Recall wins).
	Scope *MemoryScope
	// Recall selects one or more partitions (user bucket, global bucket, or union).
	// When nil, [EffectiveRecallForList] derives partitioning from Scope for backward compatibility.
	Recall *RecallScope
}

// SearchOptions configures search of long-term memory entries.
type SearchOptions struct {
	Category  MemoryCategory
	TopK      int
	Threshold float64
	// Scope, when non-nil, restricts hits to entries in that scope bucket.
	// If Recall is set, Scope is ignored for filtering (Recall wins).
	Scope *MemoryScope
	// Recall selects partitions for Search; nil derives from Scope via [EffectiveRecallForSearch].
	Recall *RecallScope
	// QueryVector, when non-nil, is passed directly to the store implementation,
	// allowing the caller to pre-compute the embedding once and share it across
	// multiple category searches. Stores that do not support vector search ignore it.
	QueryVector []float32
}

// Embedder computes vector embeddings from text.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// VectorSearcher performs similarity search over pre-indexed vectors.
type VectorSearcher interface {
	SearchByVector(ctx context.Context, runtimeID string, vec []float32, opts SearchOptions) ([]*MemoryEntry, error)
}

// DefaultGlobalCategories is used when LongTermConfig.GlobalCategories is empty.
func DefaultGlobalCategories() []MemoryCategory {
	return []MemoryCategory{CategoryProfile, CategoryPreferences, CategoryCases, CategoryPatterns}
}

func isGlobalCategory(cat MemoryCategory, global []MemoryCategory) bool {
	if len(global) == 0 {
		global = DefaultGlobalCategories()
	}
	for _, g := range global {
		if g == cat {
			return true
		}
	}
	return false
}

// NormalizeScopeForCategory returns the scope stored on an entry for the given category.
func NormalizeScopeForCategory(cat MemoryCategory, in MemoryScope, global []MemoryCategory) MemoryScope {
	out := in
	if isGlobalCategory(cat, global) {
		out.UserID = ""
		out.SessionID = ""
	}
	return out
}

// EntryMatchesQueryScope reports whether e belongs to the bucket described by query.
// query nil matches all entries (legacy stores / uncoped queries).
func EntryMatchesQueryScope(e *MemoryEntry, query *MemoryScope) bool {
	if query == nil {
		return true
	}
	// Global bucket: only runtime-global rows.
	if query.IsGlobal() {
		return e.Scope.IsGlobal()
	}
	if e.Scope.UserID != query.UserID {
		return false
	}
	if query.SessionID != "" {
		return e.Scope.SessionID == query.SessionID || e.Scope.SessionID == ""
	}
	return true
}
