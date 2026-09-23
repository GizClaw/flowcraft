package fact

import "context"

// Publication is emitted after an immutable fact publish or merge. A failed
// sink leaves the fact durable; retrying Add re-emits the same logical event.
type Publication struct {
	Fact           Fact
	Event          string
	PublicationID  string
	RevisionDigest string
}

// SourceDigestEvidence separates stored authority from an independent
// provenance rehash for repair inspection.
type SourceDigestEvidence struct {
	Name           string
	StoredDigest   string
	ComputedDigest string
}

type PublicationSink interface {
	PublishFact(context.Context, Publication) error
}
