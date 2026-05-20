package port

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

// Op identifies the canonical projection operation that triggered
// an event.
type Op string

const (
	OpProject Op = "project"
	OpForget  Op = "forget"
	OpRebuild Op = "rebuild"
)

// ProjectionEvent carries enough context for a telemetry backend
// to attribute a projection fanout outcome. Err is nil on success.
type ProjectionEvent struct {
	Projection  string
	Op          Op
	Consistency string
	FactCount   int
	Err         error
}

// DriftReason classifies a single projection-vs-canonical drift
// observation.
type DriftReason string

const (
	DriftStaleFact      DriftReason = "stale_fact"
	DriftSupersededFact DriftReason = "superseded_fact"
)

// DriftEvent describes one drift observation surfaced by
// materialization or future reconcile scanners.
type DriftEvent struct {
	Scope   domain.Scope
	Source  string
	Reason  DriftReason
	FactID  string
	Details string
}

// PipelineEvent records one high-level Save / Recall pipeline stage.
type PipelineEvent struct {
	Scope   domain.Scope
	Stage   string
	Op      string
	Count   int
	Latency time.Duration
	Err     error
}

// TelemetryHook receives lifecycle and failure signals from
// projection fanout, drift-aware components, and the Save / Recall
// pipeline.
//
// v2 carries the contract on a dual track: the legacy methods
// (OnProjection / OnDrift / OnPipeline) stay live so existing
// hook implementations keep working, while OnStage(StageDiagnostic)
// is the new single-source surface Phase B will wire. Phase E.3
// removes the legacy methods once external consumers have migrated.
type TelemetryHook interface {
	OnProjection(event ProjectionEvent)
	OnDrift(event DriftEvent)
	OnPipeline(event PipelineEvent)
	OnStage(event diagnostic.StageDiagnostic)
}
