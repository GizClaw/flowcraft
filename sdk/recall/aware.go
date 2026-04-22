package recall

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// ShortTermMemory mirrors sdk/history.Memory. Re-declared locally so
// this package compiles without an import cycle (sdk/history imports
// sdk/recall for LongTermConfig, and recall must not import history back).
type ShortTermMemory interface {
	Load(ctx context.Context, conversationID string) ([]model.Message, error)
	Append(ctx context.Context, conversationID string, newMessages []model.Message) error
	Clear(ctx context.Context, conversationID string) error
}

// LongTermConfig controls long-term memory extraction and injection.
type LongTermConfig struct {
	Enabled    bool     `json:"enabled"`
	Categories []string `json:"categories,omitempty"`
	MaxEntries int      `json:"max_entries,omitempty"`

	// PinnedCategories are always fully injected regardless of query relevance.
	// Empty uses default: [profile, preferences].
	PinnedCategories []MemoryCategory `json:"pinned_categories,omitempty"`

	// RecallCategories are searched by query relevance.
	// Empty uses default: [entities, events, cases, patterns].
	RecallCategories []MemoryCategory `json:"recall_categories,omitempty"`

	// RecallPartitions overrides which partitions each recall category searches.
	// nil or missing keys default to user-only ([PartitionUser]).
	// Example: shared "adventure" rows live in the global bucket — set
	// RecallPartitions["adventure"] = []MemoryPartition{PartitionUser, PartitionGlobal}.
	RecallPartitions map[MemoryCategory][]MemoryPartition `json:"recall_partitions,omitempty"`

	// ScopeEnabled, when true, causes ContextAssembler to use MemoryScope for
	// List/Search (requires SetScope on MemoryAwareMemory before Load).
	ScopeEnabled bool `json:"scope_enabled,omitempty"`

	// GlobalCategories are stored and queried in the runtime-global bucket
	// (empty UserID). When nil, DefaultGlobalCategories is used.
	GlobalCategories []MemoryCategory `json:"global_categories,omitempty"`
}

// MemoryAwareMemory injects long-term memory into LLM context via [ContextAssembler].
type MemoryAwareMemory struct {
	inner     ShortTermMemory
	assembler *ContextAssembler
	ltConfig  LongTermConfig

	mu        sync.RWMutex
	runtimeID string
	scope     *MemoryScope
}

// NewMemoryAwareMemory wraps inner Memory with long-term awareness.
func NewMemoryAwareMemory(inner ShortTermMemory, assembler *ContextAssembler, ltConfig LongTermConfig) *MemoryAwareMemory {
	return &MemoryAwareMemory{
		inner:     inner,
		assembler: assembler,
		ltConfig:  ltConfig,
	}
}

// NewMemoryAwareMemoryCompat preserves the pre-P0 constructor signature.
// It builds a [ContextAssembler] from ltStore and [LongTermConfig] defaults.
func NewMemoryAwareMemoryCompat(inner ShortTermMemory, ltStore LongTermStore, runtimeID string, ltConfig LongTermConfig) *MemoryAwareMemory {
	pin := ltConfig.PinnedCategories
	if len(pin) == 0 {
		pin = defaultPinnedCategories
	}
	rec := ltConfig.RecallCategories
	if len(rec) == 0 {
		rec = defaultRecallCategories
	}
	max := ltConfig.MaxEntries
	if max <= 0 {
		max = 10
	}
	assembler := NewContextAssembler(ltStore, AssemblerConfig{
		MaxEntries:       max,
		PinnedCategories: pin,
		RecallCategories: rec,
		RecallPartitions: ltConfig.RecallPartitions,
		PinnedCacheTTL:   pinnedCacheTTL,
		RecallCacheTTL:   recallCacheTTL,
		ScopeEnabled:     ltConfig.ScopeEnabled,
		GlobalCategories: ltConfig.GlobalCategories,
	})
	m := NewMemoryAwareMemory(inner, assembler, ltConfig)
	m.SetRuntimeID(runtimeID)
	return m
}

// SetRuntimeID sets the runtime ID for long-term memory lookups.
func (m *MemoryAwareMemory) SetRuntimeID(runtimeID string) {
	m.mu.Lock()
	m.runtimeID = runtimeID
	m.mu.Unlock()
}

// SetScope sets the memory scope for subsequent Load calls when ScopeEnabled.
func (m *MemoryAwareMemory) SetScope(scope *MemoryScope) {
	m.mu.Lock()
	m.scope = scope
	m.mu.Unlock()
}

// defaultPinnedCategories are always fully injected regardless of query relevance.
var defaultPinnedCategories = []MemoryCategory{CategoryProfile, CategoryPreferences}

// defaultRecallCategories are searched by query relevance.
var defaultRecallCategories = []MemoryCategory{CategoryEntities, CategoryEvents, CategoryCases, CategoryPatterns}

const pinnedLimitPerCategory = 50
const pinnedCacheTTL = 30 * time.Second
const recallCacheTTL = 10 * time.Second

const (
	recallThrottleMinInterval = 5 * time.Second
	recallThrottleMaxInterval = 30 * time.Second
	recallQueryChangeRatio    = 0.5
)

func (m *MemoryAwareMemory) Load(ctx context.Context, conversationID string) ([]model.Message, error) {
	msgs, err := m.inner.Load(ctx, conversationID)
	if err != nil {
		return nil, err
	}

	m.mu.RLock()
	runtimeID := m.runtimeID
	scope := m.scope
	m.mu.RUnlock()

	if m.assembler == nil || !m.ltConfig.Enabled || runtimeID == "" {
		return msgs, nil
	}

	ltContext, err := m.assembler.Assemble(ctx, runtimeID, scope, msgs)
	if err != nil || ltContext == "" {
		return msgs, nil
	}

	return prependSystemContext(msgs, ltContext), nil
}

// Append forwards to the underlying short-term memory. Long-term
// extraction is intentionally NOT triggered here: it happens through the
// recall.Memory.Save / Add path, on its own background pipeline.
func (m *MemoryAwareMemory) Append(ctx context.Context, conversationID string, newMessages []model.Message) error {
	return m.inner.Append(ctx, conversationID, newMessages)
}

func (m *MemoryAwareMemory) Clear(ctx context.Context, conversationID string) error {
	return m.inner.Clear(ctx, conversationID)
}

// extractQueryFromMessages builds a search query from the last N user messages,
// combining them to improve recall for short follow-up replies like "ok" or "continue".
func extractQueryFromMessages(msgs []model.Message) string {
	const maxUserMsgs = 3
	const maxQueryRunes = 500

	var parts []string
	count := 0
	for i := len(msgs) - 1; i >= 0 && count < maxUserMsgs; i-- {
		if msgs[i].Role == model.RoleUser {
			text := msgs[i].Content()
			if text != "" {
				parts = append([]string{text}, parts...)
				count++
			}
		}
	}
	query := strings.Join(parts, " ")
	if runes := []rune(query); len(runes) > maxQueryRunes {
		query = string(runes[:maxQueryRunes])
	}
	return query
}

// formatLongTermMemory formats entries into the standard injection block.
func formatLongTermMemory(entries []*MemoryEntry) string {
	var b strings.Builder
	b.WriteString("[Long-term memory]\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "- [%s] %s\n", e.Category, e.Content)
	}
	b.WriteString("[End of long-term memory]")
	return b.String()
}

// prependSystemContext injects long-term memory context into the system prompt.
func prependSystemContext(msgs []model.Message, ltContext string) []model.Message {
	result := make([]model.Message, 0, len(msgs)+1)

	if len(msgs) > 0 && msgs[0].Role == model.RoleSystem {
		existingSystem := msgs[0].Content()
		result = append(result, model.NewTextMessage(model.RoleSystem,
			ltContext+"\n\n"+existingSystem))
		result = append(result, msgs[1:]...)
	} else {
		result = append(result, model.NewTextMessage(model.RoleSystem, ltContext))
		result = append(result, msgs...)
	}
	return result
}

func queryChangeRatio(old, new string) float64 {
	oldRunes := []rune(old)
	newRunes := []rune(new)
	total := max(len(oldRunes), len(newRunes))
	if total == 0 {
		return 0
	}
	shared := 0
	limit := min(len(oldRunes), len(newRunes))
	for i := 0; i < limit; i++ {
		if oldRunes[i] == newRunes[i] {
			shared++
		}
	}
	return 1.0 - float64(shared)/float64(total)
}
