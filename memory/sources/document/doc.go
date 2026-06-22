// Package document stores canonical external document evidence.
//
// The package is intentionally limited to source-level persistence of raw
// documents and their provenance. Higher-level behaviors such as chunking,
// chunk offsets, page references, extraction, summaries, retrieval projections,
// embeddings, scores, and worker lifecycle orchestration
// are derived layers built on top of this evidence source by other packages.
//
// Document metadata is stored as JSON and decoded with encoding/json semantics.
// Metadata values should therefore be JSON-compatible; JSON numbers decode as
// float64 when read back into map[string]any.
package document
