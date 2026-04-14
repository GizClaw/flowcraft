// Package workflow defines the execution blackboard (Board: Vars + Channels),
// the Runtime orchestration API (Run, MemorySession, prepare/finish),
// Agent/Strategy/Memory abstractions, and Request/Result types.
//
// Graph execution Strategy lives in subpackage workflow/flowgraph (imports graph).
// Callers typically construct a Runtime with WithPrepareBoard for platform-specific
// board setup and WithDependencies for Factory + Executor when using flowgraph.
package workflow
