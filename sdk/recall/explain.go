package recall

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

// RecallTrace is the read-path explain surface. Phase E.3 made it
// Stages-only: every observable signal (plan, sources, drops, fused
// pool size, materialized count, latency) is reconstructable from
// trace.Stages via sdk/recall/diagnostics.
type RecallTrace = domain.RecallTrace

// SaveTrace is the write-path explain surface (Phase E.3: Stages-only).
type SaveTrace = domain.SaveTrace

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
	DropRetired        = diagnostic.DropRetired
)

// StructurizerCoverage is the compiler Structurizer tally surface.
type StructurizerCoverage = diagnostic.StructurizerCoverage

// RecallExplainer is the opt-in extension that returns a RecallTrace
// alongside hits. Memory implementations that support explain can be
// type-asserted to this interface.
type RecallExplainer interface {
	RecallExplain(ctx context.Context, scope Scope, query Query) ([]Hit, RecallTrace, error)
}

// SaveExplainer is the opt-in extension that returns a SaveTrace
// alongside SaveResult. Memory implementations that support explain
// can be type-asserted; callers without diagnostics use Memory.Save.
type SaveExplainer interface {
	SaveExplain(ctx context.Context, scope Scope, req SaveRequest) (SaveResult, SaveTrace, error)
}

// SaveDebugExplainer returns traces that may include raw fact payloads
// in dropped-fact diagnostics. Output is not covered by ForgetAll
// hard-delete lifecycle guarantees — use only for local debugging.
type SaveDebugExplainer interface {
	SaveExplainDebug(ctx context.Context, scope Scope, req SaveRequest) (SaveResult, SaveTrace, error)
}
