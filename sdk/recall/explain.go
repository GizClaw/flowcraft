package recall

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// RecallTrace is the structured explanation of a single Recall call.
// It mirrors internal/model.RecallTrace so the public surface stays
// in lockstep with the canonical model.
type RecallTrace = model.RecallTrace

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
