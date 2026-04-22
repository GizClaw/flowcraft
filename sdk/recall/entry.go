package recall

import (
	"context"
	"sync"
	"time"
)

// Category classifies a long-term memory entry.
type Category string

const (
	CategoryProfile     Category = "profile"
	CategoryPreferences Category = "preferences"
	CategoryEntities    Category = "entities"
	CategoryEvents      Category = "events"
	CategoryCases       Category = "cases"
	CategoryPatterns    Category = "patterns"

	// Categories added by (Additive Extraction taxonomy).
	CategoryProcedural Category = "procedural"
	CategoryEpisodic   Category = "episodic"
	CategorySemantic   Category = "semantic"
)

var defaultCategories = []Category{
	CategoryProfile, CategoryPreferences, CategoryEntities,
	CategoryEvents, CategoryCases, CategoryPatterns,
}

var categoryRegistry struct {
	mu         sync.RWMutex
	registered []Category
}

// RegisterCategory adds a custom memory category to the global registry.
// Must be called before pipeline usage (typically in init or startup).
func RegisterCategory(cat Category) {
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
func AllCategories() []Category {
	categoryRegistry.mu.RLock()
	extra := categoryRegistry.registered
	categoryRegistry.mu.RUnlock()

	if len(extra) == 0 {
		return append([]Category(nil), defaultCategories...)
	}
	all := make([]Category, 0, len(defaultCategories)+len(extra))
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

// Scope identifies a bucket of long-term memory rows. It serves both as
// the persistence key (which namespace/agent/user a Save lands in) and as
// the read filter (which buckets a List/Search visits, via Partitions).
//
// Persistence dimensions:
//
//   - RuntimeID: scopes everything to one process/tenant (mandatory in
//     practice; empty falls back to "anon").
//   - AgentID: soft-isolation stored in entry metadata. Empty means
//     "shared across agents"; non-empty narrows recall to that agent
//     plus shared rows.
//   - UserID: per-end-user partition. Empty means "runtime-global", a
//     bucket used for facts that should be visible to every user (e.g.
//     a knowledge-base entry shared by every chat in this runtime).
//
// Recall dimension:
//
//   - Partitions selects which buckets a List/Search visits. nil/empty
//     means "auto": global scopes recall the global bucket only, and
//     non-global scopes recall the user bucket only. Set explicitly when
//     a single call must union both buckets, e.g. when shared and
//     per-user facts both contribute to the answer.
//
// Note: SessionID was intentionally removed in v0.2.0. It conflated
// conversation-thread state (which belongs in sdk/history) with
// long-term recall, and silently leaked thread IDs into namespace and
// id-hash inputs. Callers that previously partitioned LTM by session
// should either move that data to sdk/history (transient) or model it
// as a tag on Entry.Keywords (durable).
type Scope struct {
	RuntimeID  string      `json:"runtime_id,omitempty"`
	AgentID    string      `json:"agent_id,omitempty"`
	UserID     string      `json:"user_id,omitempty"`
	Partitions []Partition `json:"partitions,omitempty"`
}

// IsGlobal returns true when this scope addresses the runtime-wide bucket.
func (s Scope) IsGlobal() bool {
	return s.UserID == ""
}

// CacheKey returns a stable key suitable for in-process caching of
// recall results (callers building their own assembler/cache layer can
// use this to invalidate per-scope).
func (s Scope) CacheKey() string {
	parts := s.EffectivePartitions()
	pk := ""
	for i, p := range parts {
		if i > 0 {
			pk += ","
		}
		pk += string(p)
	}
	if s.UserID != "" {
		return s.RuntimeID + "|" + s.UserID + "|" + pk
	}
	return s.RuntimeID + "|" + pk
}

// EffectivePartitions returns Partitions if non-empty, else the auto-
// derived single-partition slice based on IsGlobal.
func (s Scope) EffectivePartitions() []Partition {
	if len(s.Partitions) > 0 {
		return NormalizePartitions(s.Partitions)
	}
	if s.IsGlobal() {
		return []Partition{PartitionGlobal}
	}
	return []Partition{PartitionUser}
}

// Entry is a single long-term memory record.
type Entry struct {
	ID        string    `json:"id"`
	Category  Category  `json:"category"`
	Content   string    `json:"content"`
	Keywords  []string  `json:"keywords,omitempty"`
	Source    Source    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Scope     Scope     `json:"scope,omitempty"`

	Categories []string   `json:"categories,omitempty"` // multi-label
	Entities   []string   `json:"entities,omitempty"`   // linked entities
	Confidence float64    `json:"confidence,omitempty"` // LLM-reported [0,1]
	ExpiresAt  *time.Time `json:"expires_at,omitempty"` // soft TTL; nil = never
}

// Source records the origin of a memory entry.
type Source struct {
	RuntimeID      string    `json:"runtime_id,omitempty"`
	ConversationID string    `json:"conversation_id,omitempty"`
	Timestamp      time.Time `json:"timestamp,omitempty"`
}

// Store is the persistence interface for runtime-scoped long-term memory.
type Store interface {
	Save(ctx context.Context, runtimeID string, entry *Entry) error
	List(ctx context.Context, runtimeID string, opts ListOptions) ([]*Entry, error)
	Search(ctx context.Context, runtimeID string, query string, opts SearchOptions) ([]*Entry, error)
	Update(ctx context.Context, runtimeID string, entry *Entry) error
	Delete(ctx context.Context, runtimeID, entryID string) error
}

// ListOptions configures listing of long-term memory entries. Scope is the
// single source of truth for both filtering and partition selection — set
// Scope.Partitions when you need to override the auto-derived behavior
// (see [Scope.EffectivePartitions]).
type ListOptions struct {
	Category Category
	Limit    int
	Scope    *Scope
}

// SearchOptions configures search of long-term memory entries. The Scope
// contract matches [ListOptions].
type SearchOptions struct {
	Category  Category
	TopK      int
	Threshold float64
	Scope     *Scope
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
	SearchByVector(ctx context.Context, runtimeID string, vec []float32, opts SearchOptions) ([]*Entry, error)
}

// DefaultGlobalCategories names the categories whose entries are stored
// in the runtime-global bucket regardless of caller scope. Callers
// constructing their own context-injection layer can use it as a sane
// default for which categories should be shared across users.
func DefaultGlobalCategories() []Category {
	return []Category{CategoryProfile, CategoryPreferences, CategoryCases, CategoryPatterns}
}

func isGlobalCategory(cat Category, global []Category) bool {
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

// NormalizeScopeForCategory returns the scope under which an entry of
// category cat should be persisted. Global categories collapse UserID
// to "" so the entry lands in the shared bucket.
func NormalizeScopeForCategory(cat Category, in Scope, global []Category) Scope {
	out := in
	if isGlobalCategory(cat, global) {
		out.UserID = ""
	}
	return out
}

// EntryMatchesScope reports whether e satisfies the persistence and
// partition filters in query. Nil query matches all entries (used by
// legacy/uncoped paths).
func EntryMatchesScope(e *Entry, query *Scope) bool {
	if query == nil {
		return true
	}
	for _, p := range query.EffectivePartitions() {
		switch p {
		case PartitionGlobal:
			if e.Scope.IsGlobal() {
				return true
			}
		case PartitionUser:
			if query.UserID != "" && e.Scope.UserID == query.UserID {
				return true
			}
		}
	}
	return false
}
