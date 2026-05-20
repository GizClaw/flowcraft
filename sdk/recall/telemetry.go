package recall

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// StageDiagnostic is the structured per-stage observation emitted
// by the v2 pipeline framework. TelemetryHook.OnStage receives one
// per pipeline stage; TelemetryHook implementers should treat the
// type as opaque except for forwarding it to their backend.
type StageDiagnostic = diagnostic.StageDiagnostic

// ProjectionEvent describes one projection fanout outcome.
type ProjectionEvent = port.ProjectionEvent

// DriftReason classifies projection-vs-canonical drift observations.
type DriftReason = port.DriftReason

const (
	DriftStaleFact      = port.DriftStaleFact
	DriftSupersededFact = port.DriftSupersededFact
)

// DriftEvent describes one drift observation surfaced by the read path or
// reconcile tooling.
type DriftEvent = port.DriftEvent

// PipelineEvent records one high-level Save/Recall stage.
type PipelineEvent = port.PipelineEvent

// TelemetryHook receives projection, drift, and pipeline events.
type TelemetryHook = port.TelemetryHook
