package domain

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

// Resolution is the output of the ConflictResolver. It separates two
// disjoint outcomes so the write pipeline can execute them
// transactionally:
//
//   - Facts: the facts that should be appended to the ledger
//     verbatim. Already includes any Supersedes pointers populated
//     by the resolver.
//   - Closes: previously-stored facts whose validity must be closed
//     after a successful Append. Each entry carries scope, fact id,
//     the ValidTo timestamp to write, and the new fact id that
//     supersedes it (becomes CorrectedBy).
//   - Drops: facts the resolver discarded (noop / dedupe), with a
//     structured reason for trace / telemetry.
type Resolution struct {
	Facts  []TemporalFact
	Closes []ValidityClose
	Drops  []diagnostic.DroppedFact
}

// ValidityClose instructs the write pipeline to close an existing
// fact's validity after the new facts have been appended.
type ValidityClose struct {
	Scope       Scope
	FactID      string
	ValidTo     time.Time
	CorrectedBy string
}
