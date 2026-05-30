// Package knowledgenode implements the "knowledge" graph node and exposes
// a Register helper for binding it into a node.Factory.
//
// Deprecated: memory-domain graph nodes are moving out of the foundational sdk.
// Use github.com/GizClaw/flowcraft/memory/knowledge directly or a memory-owned
// graph node once available. This package will be removed in v0.5.0.
//
// The node fans search queries out across the configured datasets via a
// knowledge.Service and writes the resulting hits onto the board under
// "hits" / "by_dataset" (typed) and "results" (compat projection consumed
// by graphs that predate the typed hits API).
package knowledgenode
