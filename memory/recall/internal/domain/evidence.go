package domain

import "time"

// EvidenceRef points back to source material used to produce a fact.
type EvidenceRef struct {
	ID            string
	MessageID     string
	ObservationID string
	SpanID        string
	RequestID     string
	SessionID     string
	Role          string
	Speaker       string
	Text          string
	Timestamp     time.Time
}
