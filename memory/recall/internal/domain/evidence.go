package domain

import "time"

// EvidenceRef points back to source material used to produce a fact.
type EvidenceRef struct {
	ID        string
	MessageID string
	Role      string
	Text      string
	Timestamp time.Time
}
