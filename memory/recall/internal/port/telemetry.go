package port

import "github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"

// TelemetryHook receives structured pipeline stage diagnostics. Phase
// E.3 collapsed the previous three-channel surface (OnPipeline /
// OnProjection / OnDrift) into a single OnStage method; all derived
// views are reconstructed from trace.Stages.
type TelemetryHook interface {
	OnStage(event diagnostic.StageDiagnostic)
}
