package telemetry

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// Op identifies the canonical projection operation that triggered an event.
type Op string

const (
	OpProject Op = "project"
	OpForget  Op = "forget"
	OpRebuild Op = "rebuild"
)

// ProjectionEvent carries enough context for a telemetry backend to
// attribute a projection fanout outcome. Err is nil on success.
type ProjectionEvent struct {
	Projection  string
	Op          Op
	Consistency string
	FactCount   int
	Err         error
}

// DriftReason classifies a single projection-vs-canonical drift observation.
type DriftReason string

const (
	DriftStaleFact      DriftReason = "stale_fact"
	DriftSupersededFact DriftReason = "superseded_fact"
)

// DriftEvent describes one drift observation surfaced by materialization or
// future reconcile scanners.
type DriftEvent struct {
	Scope   model.Scope
	Source  string
	Reason  DriftReason
	FactID  string
	Details string
}

// PipelineEvent records one high-level Save/Recall pipeline stage.
type PipelineEvent struct {
	Scope   model.Scope
	Stage   string
	Op      string
	Count   int
	Latency time.Duration
	Err     error
}

// Hook receives lifecycle and failure signals from projection fanout,
// drift-aware components, and the Save/Recall pipeline.
type Hook interface {
	OnProjection(event ProjectionEvent)
	OnDrift(event DriftEvent)
	OnPipeline(event PipelineEvent)
}

// NopHook is the zero-cost default telemetry hook.
type NopHook struct{}

func (NopHook) OnProjection(ProjectionEvent) {}
func (NopHook) OnDrift(DriftEvent)           {}
func (NopHook) OnPipeline(PipelineEvent)     {}
