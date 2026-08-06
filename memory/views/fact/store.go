package fact

import (
	"context"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

// Store exposes only ADD operations for facts.
type Store interface {
	Add(context.Context, AddRequest) (Fact, error)
	Get(context.Context, sdkmemory.Scope, string, string) (Fact, bool, error)
	List(context.Context, sdkmemory.Scope, string, ListOptions) ([]Fact, error)
	ListScope(context.Context, sdkmemory.Scope) ([]Fact, error)
	ListPublications(context.Context, sdkmemory.Scope) ([]Publication, error)
}

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
