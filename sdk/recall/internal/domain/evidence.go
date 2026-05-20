package domain

import "time"

// EvidenceRef points back to source material used to produce a fact.
// Phase 1 keeps evidence embedded; a SourceEvidenceStore adapter lands
// in a later phase without breaking this shape.
type EvidenceRef struct {
	ID        string
	MessageID string
	Role      string
	Text      string
	Timestamp time.Time
}
