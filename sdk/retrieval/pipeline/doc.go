// Package pipeline composes pluggable retrieval Stages over a retrieval.Index
// .
//
// Deprecated: use github.com/GizClaw/flowcraft/memory/retrieval/pipeline instead.
// This package will be removed in v0.5.0.
//
// A Pipeline is a linear list of Stages run sequentially over a shared State.
// Empty inputs propagate as no-ops: fusion stages skip when State.Recalls
// is empty, boost / decay / threshold / post-filter / limit stages skip
// when State.Final / Reranked / Fused are all empty. This keeps single-
// lane pipelines that omit recall (e.g. caller pre-populates Final) safe
// to compose without per-stage guards.
//
// Stages may short-circuit by setting State.ShortCircuit = true (e.g. native
// hybrid backends).
package pipeline
