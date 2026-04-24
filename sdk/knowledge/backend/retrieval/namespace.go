// Package retrieval implements knowledge.ChunkRepo and
// knowledge.LayerRepo on top of any retrieval.Index.
//
// Namespace strategy (Q7=A): each dataset gets its own pair of
// namespaces, so backends with native namespace isolation (Postgres
// partitions, SQLite per-namespace tables) can scale by dataset
// without the knowledge layer doing scatter/gather inside one big
// index.
//
//	kb_<sane(dataset)>__chunks   // DerivedChunks for one dataset
//	kb_<sane(dataset)>__layers   // DerivedLayers for one dataset
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
package retrieval

import "strings"

// namespacePrefix prefixes every knowledge namespace so they can coexist
// with other consumers (recall, history, ...) of the same retrieval.Index.
const namespacePrefix = "kb_"

// chunksSuffix is appended to dataset namespaces holding DerivedChunks.
const chunksSuffix = "__chunks"

// layersSuffix is appended to dataset namespaces holding DerivedLayers.
const layersSuffix = "__layers"

// chunksNamespace returns the namespace for the dataset's chunks.
func chunksNamespace(datasetID string) string {
	return namespacePrefix + sanitiseDatasetID(datasetID) + chunksSuffix
}

// layersNamespace returns the namespace for the dataset's layers.
func layersNamespace(datasetID string) string {
	return namespacePrefix + sanitiseDatasetID(datasetID) + layersSuffix
}

// sanitiseDatasetID mirrors recall.saneNS so the namespaces produced
// here are accepted by every retrieval backend (Postgres / SQLite
// validation rejects non [A-Za-z0-9_] characters).
func sanitiseDatasetID(s string) string {
	if s == "" {
		return "anon"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "anon"
	}
	return b.String()
}
