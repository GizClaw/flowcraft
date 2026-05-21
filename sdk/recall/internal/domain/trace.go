package domain

import "github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"

// RecallTrace is the read-path explain surface. Phase E.3 made it
// Stages-only: every observable signal (plan, sources, drops, fused
// pool size, materialized count, latency, reranker outcome) is
// reconstructable from Stages via sdk/recall/diagnostics.
type RecallTrace struct {
	Stages []diagnostic.StageDiagnostic
}

// SaveTrace is the write-path explain surface (Phase E.3: Stages-only).
type SaveTrace struct {
	Stages []diagnostic.StageDiagnostic
}
