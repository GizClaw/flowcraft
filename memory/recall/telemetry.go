package recall

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// StageDiagnostic is the structured per-stage observation emitted
// by the v2 pipeline framework.
type StageDiagnostic = diagnostic.StageDiagnostic

// TelemetryHook receives structured pipeline stage diagnostics.
type TelemetryHook = port.TelemetryHook
