// Package memory exposes a public system facade for assembling declarative
// memory specs with caller-provided stores, indexes, and services.
//
// System is the public control plane: Spec + Plan + JobStore + Lifecycle
// runners + Diagnostic probes + internal Executor. High-level write, read,
// lifecycle, and diagnostics methods are driven by a root facade Plan compiled
// from Spec writeStages/readStages/lifecycle/diagnostics. AppendMessage appends
// canonical messages before running sync derivation stages or enqueueing async
// write chains, ImportDocument stores canonical documents before deriving
// document chunks, PackContext turns one product query into the retrieval
// requests selected by read stages, lifecycle methods expose readiness and
// queue control, and diagnostics stages dispatch registered probes. Empty stage
// lists use conservative capability-based defaults. Low-level vertical
// execution remains internal to the system.
package memory
