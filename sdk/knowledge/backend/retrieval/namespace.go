// Package retrieval implements knowledge.ChunkRepo and
// knowledge.LayerRepo on top of any retrieval.Index.
//
// Namespace strategy (Q7=A): each dataset gets its own triple of
// namespaces, so backends with native namespace isolation (Postgres
// partitions, SQLite per-namespace tables) can scale by dataset
// without the knowledge layer doing scatter/gather inside one big
// index.
//
//	kb_<sane(dataset)>__chunks   // DerivedChunks for one dataset
//	kb_<sane(dataset)>__layers   // DerivedLayers for one dataset
//	kb_<sane(dataset)>__docs     // doc-level virtual documents (#134)
//
// The __docs namespace holds one retrieval.Doc per logical document;
// its Content is the concatenation of the document's chunks. It
// exists so SearchDocs can hit a doc-granular inverted index with
// real doc-level corpus statistics (N = doc count, avgdl = average
// doc length), rather than collapsing chunk-level BM25 scores at
// query time — see #134 for the math reason the latter cannot meet
// doc-level BEIR / MS-MARCO acceptance criteria.
//
// Cross-dataset Search (q.DatasetIDs == nil) is a fan-out across
// every dataset id supplied by the caller. The Index interface does
// not expose "enumerate namespaces", so callers MUST resolve the
// dataset id list themselves (typically via DocumentRepo.ListDatasets)
// before invoking Search; passing an empty slice yields no results.
//
// Metadata schema written into retrieval.Doc:
//
//	dataset_id   string
//	doc_name     string
//	chunk_index  int     (chunks only; -1 for layers)
//	layer        string  ("L0" / "L1" / "L2")
//	scope        string  (layers only: "doc" or "dataset")
//	source_ver   uint64  (DerivedSig.SourceVer)
//	chunker_sig  string  (DerivedSig.ChunkerSig; chunks only)
//	prompt_sig   string  (DerivedSig.PromptSig;  layers only)
//	embed_sig    string  (DerivedSig.EmbedSig)
//
// Deprecated: use github.com/GizClaw/flowcraft/memory/knowledge/backend/retrieval
// instead. This package will be removed in v0.5.0.
package retrieval

import retrievalns "github.com/GizClaw/flowcraft/sdk/retrieval/namespace"

var knowledgeNamespace = retrievalns.MustRegister("kb")

// chunksSuffix is appended to dataset namespaces holding DerivedChunks.
const chunksSuffix = "chunks"

// layersSuffix is appended to dataset namespaces holding DerivedLayers.
const layersSuffix = "layers"

// docsSuffix is appended to dataset namespaces holding the doc-level
// virtual documents used by SearchDocs. See package doc for the
// motivation (#134).
const docsSuffix = "docs"

// chunksNamespace returns the namespace for the dataset's chunks.
func chunksNamespace(datasetID string) string {
	return knowledgeNamespace.DatasetScope(datasetID, chunksSuffix)
}

// layersNamespace returns the namespace for the dataset's layers.
func layersNamespace(datasetID string) string {
	return knowledgeNamespace.DatasetScope(datasetID, layersSuffix)
}

// docsNamespace returns the namespace for the dataset's doc-level
// virtual documents (#134). Kept package-private: callers must go
// through SearchDocs / Replace; touching the namespace directly
// would let stale doc-level data drift from the chunks namespace.
func docsNamespace(datasetID string) string {
	return knowledgeNamespace.DatasetScope(datasetID, docsSuffix)
}

// sanitiseDatasetID mirrors recall.saneNS so the namespaces produced
// here are accepted by every retrieval backend (Postgres / SQLite
// validation rejects non [A-Za-z0-9_] characters).
//
// Deprecated: use retrieval/namespace.Sanitize. This compatibility shim will
// be removed in v0.5.0 after knowledge namespace construction is fully
// centralised in sdk/retrieval/namespace.
func sanitiseDatasetID(s string) string {
	return retrievalns.Sanitize(s)
}
