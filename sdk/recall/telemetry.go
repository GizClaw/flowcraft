package recall

import "github.com/GizClaw/flowcraft/sdk/recall/internal/telemetry"

// ProjectionEvent describes one projection fanout outcome.
type ProjectionEvent = telemetry.ProjectionEvent

// DriftReason classifies projection-vs-canonical drift observations.
type DriftReason = telemetry.DriftReason

const (
	DriftStaleFact      = telemetry.DriftStaleFact
	DriftSupersededFact = telemetry.DriftSupersededFact
)

// DriftEvent describes one drift observation surfaced by the read path or
// reconcile tooling.
type DriftEvent = telemetry.DriftEvent

// PipelineEvent records one high-level Save/Recall stage.
type PipelineEvent = telemetry.PipelineEvent

// TelemetryHook receives projection, drift, and pipeline events.
type TelemetryHook = telemetry.Hook
