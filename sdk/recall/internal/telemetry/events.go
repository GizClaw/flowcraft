package telemetry

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// NopHook is the zero-cost default telemetry hook. It implements
// the dual-track port.TelemetryHook contract: the legacy
// per-subsystem methods plus the v2 OnStage(StageDiagnostic)
// surface (Phase A.2 C7).
type NopHook struct{}

var _ port.TelemetryHook = NopHook{}

func (NopHook) OnProjection(port.ProjectionEvent)     {}
func (NopHook) OnDrift(port.DriftEvent)               {}
func (NopHook) OnPipeline(port.PipelineEvent)         {}
func (NopHook) OnStage(diagnostic.StageDiagnostic)    {}
