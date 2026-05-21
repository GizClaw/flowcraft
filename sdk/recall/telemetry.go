package recall

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// StageDiagnostic is the structured per-stage observation emitted
// by the v2 pipeline framework.
type StageDiagnostic = diagnostic.StageDiagnostic

// TelemetryHook receives structured pipeline stage diagnostics
// (Phase E.3: single-rail surface).
type TelemetryHook = port.TelemetryHook
