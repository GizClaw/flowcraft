package telemetry

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// NopHook is the zero-cost default telemetry hook.
type NopHook struct{}

var _ port.TelemetryHook = NopHook{}

func (NopHook) OnStage(diagnostic.StageDiagnostic) {}
