// Package sqlite provides a retrieval.Index backed by a single SQLite database
// file.
//
// Each namespace maps to a pair of tables:
//
//	docs_<ns>  — primary store (id, content, metadata JSON, vector blob, sparse blob, ts)
//	fts_<ns>   — FTS5 virtual table over content (BM25)
//
// Vector search is performed client-side in the Pipeline; this adapter does
// NOT advertise Hybrid (Capabilities.Hybrid=false)
//
// Implementation uses modernc.org/sqlite (pure Go, no cgo) so it works on all
// platforms supported by the rest of the SDK.
package sqlite
