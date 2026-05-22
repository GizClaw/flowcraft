// Package history is deprecated.
//
// Deprecated: use github.com/GizClaw/flowcraft/memory/history instead.
// This compatibility package will be removed in v0.5.0.
package history

import target "github.com/GizClaw/flowcraft/memory/history"

type (
	ArchiveConfig          = target.ArchiveConfig
	ArchiveManifest        = target.ArchiveManifest
	ArchiveResult          = target.ArchiveResult
	ArchiveSegment         = target.ArchiveSegment
	Budget                 = target.Budget
	BufferOption           = target.BufferOption
	CompactConfig          = target.CompactConfig
	CompactOption          = target.CompactOption
	CompactResult          = target.CompactResult
	Coordinator            = target.Coordinator
	DAGConfig              = target.DAGConfig
	EstimateCounter        = target.EstimateCounter
	FileStore              = target.FileStore
	FileSummaryStore       = target.FileSummaryStore
	FileSummaryStoreOption = target.FileSummaryStoreOption
	FilterableHistory      = target.FilterableHistory
	History                = target.History
	InMemoryStore          = target.InMemoryStore
	InMemoryStoreOption    = target.InMemoryStoreOption
	LoadOptions            = target.LoadOptions
	MessageAppender        = target.MessageAppender
	RangeReader            = target.RangeReader
	RecentReader           = target.RecentReader
	Store                  = target.Store
	SummaryDAG             = target.SummaryDAG
	SummaryListOptions     = target.SummaryListOptions
	SummaryNode            = target.SummaryNode
	SummaryStore           = target.SummaryStore
	TiktokenCounter        = target.TiktokenCounter
	TokenCounter           = target.TokenCounter
)

var (
	ErrClosed                      = target.ErrClosed
	ApplyLoadOptions               = target.ApplyLoadOptions
	Archive                        = target.Archive
	BuildSummaryIndex              = target.BuildSummaryIndex
	ConversationIDFrom             = target.ConversationIDFrom
	DefaultDAGConfig               = target.DefaultDAGConfig
	LoadArchivedMessages           = target.LoadArchivedMessages
	LoadFiltered                   = target.LoadFiltered
	LoadManifest                   = target.LoadManifest
	NewBuffer                      = target.NewBuffer
	NewCompacted                   = target.NewCompacted
	NewFileStore                   = target.NewFileStore
	NewFileSummaryStore            = target.NewFileSummaryStore
	NewInMemoryStore               = target.NewInMemoryStore
	NewSummaryDAG                  = target.NewSummaryDAG
	NewSummaryNodeID               = target.NewSummaryNodeID
	NewTiktokenCounter             = target.NewTiktokenCounter
	NewTiktokenCounterFromEncoding = target.NewTiktokenCounterFromEncoding
	WithArchiveBatchSize           = target.WithArchiveBatchSize
	WithArchiveThreshold           = target.WithArchiveThreshold
	WithBufferMax                  = target.WithBufferMax
	WithChunkSize                  = target.WithChunkSize
	WithCompactThreshold           = target.WithCompactThreshold
	WithCondenseThreshold          = target.WithCondenseThreshold
	WithConversationID             = target.WithConversationID
	WithDAGConfig                  = target.WithDAGConfig
	WithLeafPrune                  = target.WithLeafPrune
	WithMaxConversations           = target.WithMaxConversations
	WithMaxDepth                   = target.WithMaxDepth
	WithRecentRatio                = target.WithRecentRatio
	WithStoragePrefix              = target.WithStoragePrefix
	WithSummaryStoreCapacity       = target.WithSummaryStoreCapacity
	WithTTL                        = target.WithTTL
	WithTokenBudget                = target.WithTokenBudget
	WithTokenCounter               = target.WithTokenCounter
)
