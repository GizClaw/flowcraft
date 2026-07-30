// Package factory wires the canonical knowledge.Service stack. It
// lives in a subpackage to break the import cycle that would arise if
// the top-level knowledge package depended on backend/retrieval
// (which depends on knowledge for the repo interfaces).
//
// NewRetrieval is the single entry point: chunks/layers live inside
// an existing retrieval.Index while documents stay in the supplied
// DocumentRepo.
package factory

import (
	"github.com/GizClaw/flowcraft/memory/knowledge"
	knowledgeretrieval "github.com/GizClaw/flowcraft/memory/knowledge/backend/retrieval"
	"github.com/GizClaw/flowcraft/memory/retrieval"
)

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
