package domain

import "github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"

// RecallTrace is the read-path explain surface. Every observable signal (plan,
// sources, drops, fused pool size, materialized count, latency, reranker
// outcome) is reconstructable from Stages via sdk/recall/diagnostics.
type RecallTrace struct {
	Stages []diagnostic.StageDiagnostic
}

// SaveTrace is the write-path explain surface.
type SaveTrace struct {
	Stages []diagnostic.StageDiagnostic
}
