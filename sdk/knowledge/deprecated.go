// Package knowledge is deprecated.
//
// Deprecated: use github.com/GizClaw/flowcraft/memory/knowledge instead.
// This compatibility package will be removed in v0.5.0.
package knowledge

import target "github.com/GizClaw/flowcraft/memory/knowledge"

type (
	BM25Retriever     = target.BM25Retriever
	CJKTokenizer      = target.CJKTokenizer
	Candidate         = target.Candidate
	ChangeEvent       = target.ChangeEvent
	ChunkConfig       = target.ChunkConfig
	ChunkQuery        = target.ChunkQuery
	ChunkRepo         = target.ChunkRepo
	ChunkSigReader    = target.ChunkSigReader
	ChunkSpec         = target.ChunkSpec
	Chunker           = target.Chunker
	ContextLayer      = target.ContextLayer
	CorpusStats       = target.CorpusStats
	DatasetContext    = target.DatasetContext
	DerivedChunk      = target.DerivedChunk
	DerivedLayer      = target.DerivedLayer
	DerivedSig        = target.DerivedSig
	DocLevelSearcher  = target.DocLevelSearcher
	DocVectorSearcher = target.DocVectorSearcher
	DocumentContext   = target.DocumentContext
	DocumentRepo      = target.DocumentRepo
	DocumentSummary   = target.DocumentSummary
	Embedder          = target.Embedder
	EventKind         = target.EventKind
	EventNotifier     = target.EventNotifier
	EventReloader     = target.EventReloader
	Hit               = target.Hit
	Layer             = target.Layer
	LayerQuery        = target.LayerQuery
	LayerRepo         = target.LayerRepo
	LayerRetriever    = target.LayerRetriever
	Mode              = target.Mode
	Query             = target.Query
	RRFRanker         = target.RRFRanker
	Ranker            = target.Ranker
	RebuildScope      = target.RebuildScope
	Rebuilder         = target.Rebuilder
	ReloaderOptions   = target.ReloaderOptions
	Result            = target.Result
	Retriever         = target.Retriever
	Scope             = target.Scope
	SearchEngine      = target.SearchEngine
	SearchMode        = target.SearchMode
	Service           = target.Service
	ServiceOptions    = target.ServiceOptions
	SimpleTokenizer   = target.SimpleTokenizer
	SourceDocument    = target.SourceDocument
	Tokenizer         = target.Tokenizer
	VectorRetriever   = target.VectorRetriever
)

const (
	AbstractPrompt          = target.AbstractPrompt
	DatasetOverviewPrompt   = target.DatasetOverviewPrompt
	DefaultPromptInputLimit = target.DefaultPromptInputLimit
	DefaultRRFK             = target.DefaultRRFK
	DefaultThreshold        = target.DefaultThreshold
	EventBulk               = target.EventBulk
	EventDelete             = target.EventDelete
	EventPut                = target.EventPut
	LayerAbstract           = target.LayerAbstract
	LayerDetail             = target.LayerDetail
	LayerOverview           = target.LayerOverview
	ModeBM25                = target.ModeBM25
	ModeHybrid              = target.ModeHybrid
	ModeVector              = target.ModeVector
	OverviewPrompt          = target.OverviewPrompt
	ScopeAllDatasets        = target.ScopeAllDatasets
	ScopeSingleDataset      = target.ScopeSingleDataset
)

var (
	DetectTokenizer         = target.DetectTokenizer
	ExtractKeywords         = target.ExtractKeywords
	NewCorpusStats          = target.NewCorpusStats
	ScoreText               = target.ScoreText
	ChunkConfigSig          = target.ChunkConfigSig
	ChunkText               = target.ChunkText
	CosineSimilarity        = target.CosineSimilarity
	DefaultChunkConfig      = target.DefaultChunkConfig
	FuseHits                = target.FuseHits
	GenerateDatasetContext  = target.GenerateDatasetContext
	GenerateDocumentContext = target.GenerateDocumentContext
	IsValidLayer            = target.IsValidLayer
	IsValidMode             = target.IsValidMode
	NewBM25Retriever        = target.NewBM25Retriever
	NewDefaultChunker       = target.NewDefaultChunker
	NewEventReloader        = target.NewEventReloader
	NewLayerRetriever       = target.NewLayerRetriever
	NewRRFRanker            = target.NewRRFRanker
	NewSearchEngine         = target.NewSearchEngine
	NewService              = target.NewService
	NewVectorRetriever      = target.NewVectorRetriever
	ResolveMode             = target.ResolveMode
)
