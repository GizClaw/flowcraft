// Package memory defines implementation-neutral capabilities for supplying and
// accepting agent memory.
//
// The package deliberately exposes three small SPIs:
//
//   - ContextProvider supplies structured, hydrated context.
//   - TurnSink commits canonical conversation messages.
//   - DocumentSink stores normalized documents and provenance.
//
// Canonical sources, derived views, retrieval projections, lifecycle workers,
// and context-packing algorithms belong to implementations. In particular,
// vector, lexical, and entity indexes are not SDK interfaces: they are
// replaceable projections behind ContextProvider.
//
// Scope always enforces the runtime and tenant partition. Request-specific
// conversation and dataset addresses never widen that boundary.
package memory
