package memory

import (
	"github.com/GizClaw/flowcraft/sdk/memory/ltm"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// This file preserves the pre-Phase-3 surface of sdk/memory's long-term
// glue layer. All real implementations live in sdk/memory/ltm; the names
// below are type aliases / function trampolines so existing callers
// (e.g. sdkx/llm, bench/locomo) keep compiling without code changes.
//
// New code should import sdk/memory/ltm directly.

// ----------------------------------------------------------------------------
// long-term entity types
// ----------------------------------------------------------------------------

type (
	MemoryCategory = ltm.MemoryCategory
	MemoryScope    = ltm.MemoryScope
	MemoryEntry    = ltm.MemoryEntry
	MemorySource   = ltm.MemorySource
	LongTermStore  = ltm.LongTermStore
	ListOptions    = ltm.ListOptions
	SearchOptions  = ltm.SearchOptions
	Embedder       = ltm.Embedder
	VectorSearcher = ltm.VectorSearcher
)

const (
	CategoryProfile     = ltm.CategoryProfile
	CategoryPreferences = ltm.CategoryPreferences
	CategoryEntities    = ltm.CategoryEntities
	CategoryEvents      = ltm.CategoryEvents
	CategoryCases       = ltm.CategoryCases
	CategoryPatterns    = ltm.CategoryPatterns
	CategoryProcedural  = ltm.CategoryProcedural
	CategoryEpisodic    = ltm.CategoryEpisodic
	CategorySemantic    = ltm.CategorySemantic
)

// RegisterCategory adds a custom memory category to the global registry.
func RegisterCategory(cat MemoryCategory) { ltm.RegisterCategory(cat) }

// AllCategories returns all built-in and registered memory categories.
func AllCategories() []MemoryCategory { return ltm.AllCategories() }

// AllCategoryStrings returns all category names as strings.
func AllCategoryStrings() []string { return ltm.AllCategoryStrings() }

// DefaultGlobalCategories is used when LongTermConfig.GlobalCategories is empty.
func DefaultGlobalCategories() []MemoryCategory { return ltm.DefaultGlobalCategories() }

// NormalizeScopeForCategory returns the scope stored on an entry for the given category.
func NormalizeScopeForCategory(cat MemoryCategory, in MemoryScope, global []MemoryCategory) MemoryScope {
	return ltm.NormalizeScopeForCategory(cat, in, global)
}

// EntryMatchesQueryScope reports whether e belongs to the bucket described by query.
func EntryMatchesQueryScope(e *MemoryEntry, query *MemoryScope) bool {
	return ltm.EntryMatchesQueryScope(e, query)
}

// ----------------------------------------------------------------------------
// recall scope
// ----------------------------------------------------------------------------

type (
	MemoryPartition = ltm.MemoryPartition
	RecallScope     = ltm.RecallScope
)

const (
	PartitionUser   = ltm.PartitionUser
	PartitionGlobal = ltm.PartitionGlobal
)

// NormalizePartitions returns a deduplicated slice; empty input defaults to user-only.
func NormalizePartitions(parts []MemoryPartition) []MemoryPartition {
	return ltm.NormalizePartitions(parts)
}

// EffectiveRecallForList builds the active recall scope for List from ListOptions.
func EffectiveRecallForList(opts ListOptions, runtimeID string) *RecallScope {
	return ltm.EffectiveRecallForList(opts, runtimeID)
}

// EffectiveRecallForSearch is the Search counterpart of EffectiveRecallForList.
func EffectiveRecallForSearch(opts SearchOptions, runtimeID string) *RecallScope {
	return ltm.EffectiveRecallForSearch(opts, runtimeID)
}

// EntryMatchesRecallScope reports whether e belongs to any partition in r.
func EntryMatchesRecallScope(e *MemoryEntry, r *RecallScope) bool {
	return ltm.EntryMatchesRecallScope(e, r)
}

// ----------------------------------------------------------------------------
// retrieval-backed long-term store
// ----------------------------------------------------------------------------

// RetrievalLongTermStore is the canonical LongTermStore implementation
// ( Phase 3). New code should use [ltm.RetrievalStore] directly.
type RetrievalLongTermStore = ltm.RetrievalStore

// RetrievalStoreOption configures a [RetrievalLongTermStore].
type RetrievalStoreOption = ltm.RetrievalStoreOption

// WithRetrievalEmbedder enables vector lanes by embedding entries on Save and queries on Search.
func WithRetrievalEmbedder(e Embedder) RetrievalStoreOption {
	return ltm.WithRetrievalEmbedder(e)
}

// WithRetrievalPipeline overrides the default [pipeline.LTM].
var WithRetrievalPipeline = ltm.WithRetrievalPipeline

// NewRetrievalLongTermStore wires a LongTermStore to a retrieval.Index.
func NewRetrievalLongTermStore(idx retrieval.Index, opts ...RetrievalStoreOption) *RetrievalLongTermStore {
	return ltm.NewRetrievalStore(idx, opts...)
}

// ----------------------------------------------------------------------------
// context assembler + long-term-aware short-term memory
// ----------------------------------------------------------------------------

type (
	ContextAssembler  = ltm.ContextAssembler
	AssemblerConfig   = ltm.AssemblerConfig
	MemoryAwareMemory = ltm.MemoryAwareMemory
	LongTermConfig    = ltm.LongTermConfig
)

// NewContextAssembler builds an assembler. Zero TTLs default to ltm constants.
func NewContextAssembler(ltStore LongTermStore, config AssemblerConfig) *ContextAssembler {
	return ltm.NewContextAssembler(ltStore, config)
}

// NewMemoryAwareMemory wraps inner Memory with long-term awareness.
func NewMemoryAwareMemory(inner Memory, assembler *ContextAssembler, ltConfig LongTermConfig) *MemoryAwareMemory {
	return ltm.NewMemoryAwareMemory(inner, assembler, ltConfig)
}

// NewMemoryAwareMemoryCompat preserves the pre-P0 constructor signature.
func NewMemoryAwareMemoryCompat(inner Memory, ltStore LongTermStore, runtimeID string, ltConfig LongTermConfig) *MemoryAwareMemory {
	return ltm.NewMemoryAwareMemoryCompat(inner, ltStore, runtimeID, ltConfig)
}
