package port

import "github.com/GizClaw/flowcraft/memory/recall/internal/domain"

// EntitySnapshotter exposes the read-side of an entity projection
// that the write-time ingest pipeline uses to fold freshly-extracted
// mentions into existing canonical forms. Subsystems must NOT use
// reflection / type assertions to detect this capability — implement
// the port and register the implementation explicitly.
type EntitySnapshotter interface {
	Snapshot(scope domain.Scope) []EntitySnapshot
}

// EntitySnapshot is a hint about an entity the canonical projection
// has already seen in this scope. Subsystems consuming the snapshot
// (e.g. the ingest pipeline) match canonical forms case-insensitively
// to fold case / alias drift into the same canonical entity.
//
// Weight records how many sub-scopes surfaced the same canonical entity when
// the read-path plan stage merges per-sub-scope snapshots into the global
// planner input. The current rule-based planner keeps it diagnostic-only.
type EntitySnapshot struct {
	Canonical string
	Aliases   []string
	Weight    float64
}
