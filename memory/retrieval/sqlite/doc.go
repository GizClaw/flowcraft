// Package sqlite provides a retrieval.Index backed by a single SQLite database
// file.
//
// Each namespace maps to a pair of tables:
//
//	docs_<ns>  — primary store (id, content, metadata JSON, vector blob, sparse blob, ts)
//	fts_<ns>   — FTS5 virtual table over content (BM25)
//
// This adapter supports QueryText via FTS5 BM25 and SparseVec via in-process
// sparse dot-product scoring over the stored sparse blob. It does not support
// native vector scoring or multi-signal hybrid search, so it advertises
// Vector=false and Hybrid=false.
//
// Implementation uses modernc.org/sqlite (pure Go, no cgo) so it works on all
// platforms supported by the rest of the SDK.
//
// Performance note: List performs the full namespace scan in memory (read,
// filter, paginate) before returning a page. It is intended for management
// consoles and small-to-medium namespaces; bulk exports or migrations
// should drive the namespace through Iterate, which streams in id-order
// without buffering the whole result set.
package sqlite
