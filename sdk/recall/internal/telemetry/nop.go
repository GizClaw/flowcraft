package telemetry

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// NopHook is the zero-cost default telemetry hook.
type NopHook struct{}

var _ port.TelemetryHook = NopHook{}

func (NopHook) OnStage(diagnostic.StageDiagnostic) {}
