// Package factory wires the canonical knowledge.Service stacks. It
// lives in a subpackage to break the import cycle that would arise if
// the top-level knowledge package depended on backend/fs and
// backend/retrieval (both of which depend on knowledge for the repo
// interfaces).
//
// Use NewLocal for a fully filesystem-backed Service; NewRetrieval
// when chunks/layers should live inside an existing retrieval.Index.
package factory

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/knowledge/backend/fs"
	knowledgeretrieval "github.com/GizClaw/flowcraft/sdk/knowledge/backend/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/textsearch"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// LocalOption configures NewLocal.
type LocalOption func(*localConfig)

type localConfig struct {
	chunker  knowledge.Chunker
	embedder knowledge.Embedder
	embedSig string
	prefix   string
	tok      textsearch.Tokenizer
}

// WithLocalChunker overrides the chunker used by the local service.
func WithLocalChunker(c knowledge.Chunker) LocalOption {
	return func(cfg *localConfig) { cfg.chunker = c }
}

// WithLocalEmbedder enables vector-aware indexing on the local
// backend. When sig is empty, the embedder's Go type name is used as
// the EmbedSig; pass an explicit identifier for production wiring.
func WithLocalEmbedder(e knowledge.Embedder, sig string) LocalOption {
	return func(cfg *localConfig) {
		cfg.embedder = e
		cfg.embedSig = sig
	}
}

// WithLocalPrefix overrides the workspace prefix (default "knowledge").
func WithLocalPrefix(prefix string) LocalOption {
	return func(cfg *localConfig) { cfg.prefix = prefix }
}

// WithLocalTokenizer overrides the BM25 tokenizer (default CJKTokenizer).
func WithLocalTokenizer(tok textsearch.Tokenizer) LocalOption {
	return func(cfg *localConfig) { cfg.tok = tok }
}

// NewLocal assembles a knowledge.Service backed entirely by the
// workspace filesystem. ChunkRepo.Load is called eagerly so BM25
// indexes rehydrate from disk before the first search; load errors
// are swallowed (treated as "empty index").
func NewLocal(ws workspace.Workspace, opts ...LocalOption) *knowledge.Service {
	cfg := localConfig{
		prefix: fs.DefaultPrefix,
		tok:    &textsearch.CJKTokenizer{},
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.chunker == nil {
		cfg.chunker = knowledge.NewDefaultChunker(knowledge.DefaultChunkConfig())
	}
	docs := fs.NewDocumentRepo(ws, cfg.prefix)
	chunks := fs.NewChunkRepo(ws, cfg.prefix, cfg.tok)
	_ = chunks.Load(context.Background())
	layers := fs.NewLayerRepo(ws, cfg.prefix, cfg.tok)
	engine := assembleEngine(chunks, layers, cfg.embedder)
	return knowledge.NewService(docs, chunks, layers, engine, knowledge.ServiceOptions{
		Chunker:  cfg.chunker,
		Embedder: cfg.embedder,
		EmbedSig: cfg.embedSig,
	})
}

// RetrievalOption configures NewRetrieval.
type RetrievalOption func(*retrievalConfig)

type retrievalConfig struct {
	chunker  knowledge.Chunker
	embedder knowledge.Embedder
	embedSig string
}

// WithRetrievalChunker overrides the chunker.
func WithRetrievalChunker(c knowledge.Chunker) RetrievalOption {
	return func(cfg *retrievalConfig) { cfg.chunker = c }
}

// WithRetrievalEmbedder enables vector indexing inside the retrieval
// namespace. When sig is empty, the embedder's Go type name is used.
func WithRetrievalEmbedder(e knowledge.Embedder, sig string) RetrievalOption {
	return func(cfg *retrievalConfig) {
		cfg.embedder = e
		cfg.embedSig = sig
	}
}

// NewRetrieval assembles a knowledge.Service whose chunks/layers live
// inside a retrieval.Index, while documents stay in the supplied
// DocumentRepo (Q8=B: retrieval indexes are not authoritative document
// stores).
//
// Typical pairing:
//
//	docs := fs.NewDocumentRepo(ws, "knowledge")
//	idx  := memory.New()
//	svc  := factory.NewRetrieval(docs, idx)
func NewRetrieval(docs knowledge.DocumentRepo, idx retrieval.Index, opts ...RetrievalOption) *knowledge.Service {
	cfg := retrievalConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.chunker == nil {
		cfg.chunker = knowledge.NewDefaultChunker(knowledge.DefaultChunkConfig())
	}
	chunks := knowledgeretrieval.NewChunkRepo(idx)
	layers := knowledgeretrieval.NewLayerRepo(idx)
	engine := assembleEngine(chunks, layers, cfg.embedder)
	return knowledge.NewService(docs, chunks, layers, engine, knowledge.ServiceOptions{
		Chunker:  cfg.chunker,
		Embedder: cfg.embedder,
		EmbedSig: cfg.embedSig,
	})
}

// assembleEngine wires the canonical Retriever set:
//   - BM25Retriever   (always)
//   - VectorRetriever (only when an embedder is supplied)
//   - LayerRetriever  (always; vector lane gated by embedder presence)
//
// Centralised here so both factory entry points stay in sync.
func assembleEngine(chunks knowledge.ChunkRepo, layers knowledge.LayerRepo, embedder knowledge.Embedder) *knowledge.SearchEngine {
	chunkRetrievers := []knowledge.Retriever{knowledge.NewBM25Retriever(chunks)}
	if embedder != nil {
		chunkRetrievers = append(chunkRetrievers, knowledge.NewVectorRetriever(chunks, embedder))
	}
	layerRetrievers := []knowledge.Retriever{knowledge.NewLayerRetriever(layers, embedder)}
	return knowledge.NewSearchEngine(chunkRetrievers, layerRetrievers, knowledge.NewRRFRanker())
}
