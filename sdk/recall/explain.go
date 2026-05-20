package recall

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

// RecallTrace is the structured explanation of a single Recall call.
// It mirrors internal/domain.RecallTrace so the public surface stays
// in lockstep with the canonical model.
type RecallTrace = domain.RecallTrace

// QueryPlan is the planner output exposed on RecallTrace.Plan.
type QueryPlan = domain.QueryPlan

// SourceTrace describes one source's contribution to a Recall call.
type SourceTrace = domain.SourceTrace

// CandidateDrop records a candidate that did not survive the read
// path along with the stage and reason.
type CandidateDrop = diagnostic.CandidateDrop

// DropReason categorises why a candidate was dropped.
type DropReason = diagnostic.DropReason

const (
	DropStaleFact      = diagnostic.DropStaleFact
	DropDuplicate      = diagnostic.DropDuplicate
	DropTotalCap       = diagnostic.DropTotalCap
	DropPerSourceCap   = diagnostic.DropPerSourceCap
	DropSuperseded     = diagnostic.DropSuperseded
	DropMaterializeErr = diagnostic.DropMaterializeErr
	DropScopeViolation = diagnostic.DropScopeViolation
)

// RecallExplainer is the opt-in extension that returns a
// RecallTrace alongside hits. Memory implementations that support
// explain can be type-asserted to this interface; callers that
// don't need trace just use Memory.Recall directly.
type RecallExplainer interface {
	RecallExplain(ctx context.Context, scope Scope, query Query) ([]Hit, RecallTrace, error)
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
	Dropped []DroppedFact
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
	// Save call. The fields mirror the compiler-internal type so
	// they roll up cleanly into PipelineHealth across many Saves.
	StructurizerCoverage StructurizerCoverage
}

// StructurizerCoverage is the public surface of the compiler's per-
// stage Structurizer counters. Each field counts the number of facts
// that arrived with the corresponding field empty and left it non-
// empty, so the ratio against TotalFactsSeen reads as "% of facts
// that needed this sub-stage to work".
type StructurizerCoverage struct {
	// TotalFactsSeen is the number of facts the Structurizer was
	// invoked on (i.e. the denominator for every other counter).
	TotalFactsSeen int
	// KindFilled is the count of facts whose Kind the Structurizer
	// inferred from the content keyword table. High value =
	// extractor is shipping Kind=="" (legacy schema) or the LLM is
	// dropping the enum field; low value = LLM-supplied Kind owns
	// classification.
	KindFilled int
	// EntitiesFilled is the count of facts that gained at least one
	// entity via Title-Cased NER or KnownEntity substring match.
	// Tracks how much the cross-fact canonicalisation hint is
	// actually firing.
	EntitiesFilled int
	// SubjectFilled is the count of facts whose Subject was lifted
	// from turn.Speaker / first entity. Tracks how load-bearing the
	// typed Speaker channel is for SPO derivation.
	SubjectFilled int
	// ValidFromHintFilled is the count of facts that received an
	// absolute-time hint from turn.Time or the content date regex.
	// Tracks whether the timeline source is being seeded; zero
	// means the temporal pipeline is silently dead.
	ValidFromHintFilled int
}

// SaveExplainer is the opt-in extension that returns a SaveTrace
// alongside SaveResult. Memory implementations that support explain
// can be type-asserted; callers that don't need diagnostics keep
// using Memory.Save.
type SaveExplainer interface {
	SaveExplain(ctx context.Context, scope Scope, req SaveRequest) (SaveResult, SaveTrace, error)
}
