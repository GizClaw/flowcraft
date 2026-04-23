package recall

import (
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
