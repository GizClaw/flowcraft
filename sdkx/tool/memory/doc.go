// Package memory implements the Anthropic Memory Tool client-side
// contract (memory_20250818) on top of a [workspace.Workspace].
//
// # What is the Memory Tool
//
// Anthropic's Memory Tool is a client-executed tool the model uses
// to maintain a persistent file tree across turns. The model emits
// commands such as view / create / str_replace / insert / delete /
// rename and the client (this package) executes them against a
// sandboxed file store, then returns the result back to the model.
// All paths the model produces are required to begin with the
// "/memories" prefix; this package strips that prefix and routes
// the operation against the workspace root.
//
// # Why sdkx
//
// Memory Tool is a concrete protocol adapter (specific spec, fixed
// command set, fixed path prefix) — exactly the layer where sdkx
// lives. The underlying primitive ([workspace.Workspace]) stays in
// sdk; this package is the wire-level binding.
//
// # Composition with the memory hierarchy
//
// FlowCraft already exposes a four-tier memory architecture:
//
//	sdk/history    - per-conversation transcript (hot)
//	sdk/recall     - long-term facts (BM25 + vector)
//	sdk/knowledge  - retrieval over corpora (chunk + rerank)
//	sdk/workspace  - persistent file tree (Memory Tool target)
//
// This package wires the file-tree tier to the Anthropic spec so
// agents using Anthropic's client-tool calling protocol see a
// drop-in compatible "memory" surface, while internally the same
// workspace can be shared with knowledge ingestion, skills, and any
// other workspace consumer.
package memory
