// Package memory implements the Anthropic Memory Tool client-side
// contract (memory_20250818) on top of a [workspace.Workspace].
//
// Deprecated: memory-domain tool adapters are moving to
// github.com/GizClaw/flowcraft/memory. This package will be removed in v0.5.0.
//
// # What is the Memory Tool
//
// Anthropic's Memory Tool is a client-executed tool the model uses
// to maintain a persistent file tree across turns. The model emits
// commands such as view / create / str_replace / insert / delete /
// rename and the client (this package) executes them against a
// sandboxed file store, then returns the result back to the model.
// All paths the model produces are required to begin with the
// "/memories" prefix.
//
// # Path layout inside the workspace
//
// The "memories" segment is *preserved* when forwarding to the
// underlying [workspace.Workspace]: the model's "/memories/foo.md"
// becomes the workspace-relative path "memories/foo.md". The Memory
// Tool therefore lives in a dedicated subtree at
// <workspace>/memories/, peer to the other workspace consumers:
//
//	<workspace>/
//	├── memories/    <-- written by Memory Tool
//	├── retrieval/   <-- optional retrieval indexes
//	├── transcripts/ <-- optional transcript store
//	└── views/       <-- optional derived memory views
//
// This isolation is what lets a single Workspace be shared across
// workspace consumers without their writes colliding. Hosts can
// optionally wrap the workspace with [workspace.NewScopedWorkspace]
// to enforce the boundary defensively.
//
// # Why sdkx
//
// Memory Tool is a concrete protocol adapter (specific spec, fixed
// command set, fixed path prefix) — exactly the layer where sdkx
// lives. The underlying primitive ([workspace.Workspace]) stays in
// sdk; this package is the wire-level binding.
//
// # Composition with memory substrates
//
// FlowCraft exposes memory substrates that callers can combine:
//
//	memory/sources    - canonical evidence records
//	memory/views      - derived projections
//	memory/retrieval  - retrieval indexes
//	sdk/workspace     - persistent file tree (Memory Tool target)
//
// This package wires the file-tree tier to the Anthropic spec so
// agents using Anthropic's client-tool calling protocol see a
// drop-in compatible "memory" surface, while internally the same
// workspace can be shared with ingestion, skills, retrieval indexes,
// and any other workspace consumer.
package memory
