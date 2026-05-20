package domain

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

// SourceTrace captures one source's execution metrics for explain.
type SourceTrace struct {
	Source    string
	Budget    int
	Returned  int
	Truncated bool
	Latency   time.Duration
	Err       string
}

// RecallTrace is the read-path failure attribution surface. It
// stays append-only / readable; v2 telemetry feeds off the same
// fields so explain traces and metrics share a schema.
//
// Stages is the v2 structured diagnostic surface — every read-path
// stage emits a StageDiagnostic and the pipeline framework appends
// them here. The legacy flat fields (Plan / Sources / Drops / …)
// stay populated by the current procedural runner until Phase B
// makes the pipeline the sole writer; both views are in sync per
// call.
type RecallTrace struct {
	Plan            QueryPlan
	Sources         []SourceTrace
	FusedCandidates int
	Drops           []diagnostic.CandidateDrop
	Materialized    int
	// Reranked is the number of hits that the optional Reranker
	// stage actually reordered. Zero when no Reranker is wired or
	// the candidate pool was empty; equal to len(input) when the
	// reranker ran successfully on every candidate. The companion
	// RerankErr captures soft failures so operators can attribute
	// "rerank wired but didn't help" to a provider outage vs. a
	// real signal mismatch.
	Reranked     int
	RerankErr    string
	TotalLatency time.Duration

	// Stages is the structured per-stage diagnostic surface. Empty
	// until Phase B wires the pipeline framework; the legacy flat
	// fields above stay populated by the current procedural runner.
	Stages []diagnostic.StageDiagnostic
}

// SaveTrace exposes the compiled facts and write-time drops a Save
// call produced. It is the write-path counterpart of RecallTrace, so
// diagnostics can audit the extractor / compiler / resolver stages
// from the same SDK surface.
type SaveTrace struct {
	// CompiledFacts are the facts after the compiler pipeline ran
	// but before the conflict resolver decided which to append. It
	// is the right surface for "did the extractor produce useful
	// content" diagnostics — Dropped covers facts that did not make
	// it this far.
	CompiledFacts []TemporalFact
	// Appended are the facts that survived conflict resolution and
	// were actually written to the canonical store.
	Appended []TemporalFact
	// Dropped lists facts the compiler discarded with a reason
	// (policy/governance/validation).
	Dropped []diagnostic.DroppedFact
	// KnownEntitiesSeen is the number of canonical entity snapshots
	// the SDK lifted from the entity projection before this Save's
	// Compile call. It quantifies the cross-fact canonicalisation
	// hint the Structurizer had to work with: 0 on the very first
	// Save in a scope and monotonically growing thereafter. A
	// suspiciously low value mid-workload is a tell that the
	// entity projection has drifted (rebuild needed).
	KnownEntitiesSeen int
	// StructurizerCoverage breaks the compiler's Structurizer stage
	// down by sub-task (Kind / Entities / Subject / ValidFrom),
	// counting how many facts each one actually enriched on this
	// Save call. The fields mirror the canonical type that lives
	// in diagnostic/ so they roll up cleanly into PipelineHealth
	// across many Saves.
	StructurizerCoverage diagnostic.StructurizerCoverage

	// Stages is the structured per-stage diagnostic surface. Empty
	// until Phase B wires the pipeline framework; the legacy flat
	// fields above stay populated by the current procedural runner.
	Stages []diagnostic.StageDiagnostic
}
