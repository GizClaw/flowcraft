// Package sqlite provides durable memory/recall stores backed by one SQLite
// database file.
//
// A Backend shares one connection and schema across the canonical
// TemporalStore, SideEffectOutbox, and AsyncSemanticQueue adapters.
package sqlite
