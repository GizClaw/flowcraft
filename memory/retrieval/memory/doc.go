// Package memory provides MemoryIndex, the zero-dependency in-process
// retrieval.Index implementation.
//
// It is the reference backend for retrieval.Search semantics: QueryText,
// QueryVector, and SparseVec all participate in scoring, and any multi-signal
// Search is fused according to SearchRequest.HybridMode and
// SearchRequest.HybridOptions.
package memory
