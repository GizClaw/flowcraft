package recall

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// RecallTrace is the structured explanation of a single Recall call.
// It mirrors internal/model.RecallTrace so the public surface stays
// in lockstep with the canonical model.
type RecallTrace = model.RecallTrace

// QueryPlan is the planner output exposed on RecallTrace.Plan.
type QueryPlan = model.QueryPlan

// SourceTrace describes one source's contribution to a Recall call.
type SourceTrace = model.SourceTrace

// CandidateDrop records a candidate that did not survive the read
// path along with the stage and reason.
type CandidateDrop = model.CandidateDrop

// DropReason categorises why a candidate was dropped.
type DropReason = model.DropReason

const (
	DropStaleFact      = model.DropStaleFact
	DropDuplicate      = model.DropDuplicate
	DropTotalCap       = model.DropTotalCap
	DropPerSourceCap   = model.DropPerSourceCap
	DropSuperseded     = model.DropSuperseded
	DropMaterializeErr = model.DropMaterializeErr
	DropScopeViolation = model.DropScopeViolation
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
}

// SaveExplainer is the opt-in extension that returns a SaveTrace
// alongside SaveResult. Memory implementations that support explain
// can be type-asserted; callers that don't need diagnostics keep
// using Memory.Save.
type SaveExplainer interface {
	SaveExplain(ctx context.Context, scope Scope, req SaveRequest) (SaveResult, SaveTrace, error)
}
