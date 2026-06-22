package views

import "github.com/GizClaw/flowcraft/sdk/errdefs"

// ID is a stable identifier for a derived view instance.
type ID string

// Kind identifies a family of derived view.
type Kind string

const (
	KindRecentWindow   Kind = "recent_window"
	KindSummaryDAG     Kind = "summary_dag"
	KindDocumentChunks Kind = "document_chunks"
	KindMessageIndex   Kind = "message_index"
	KindEntityFacts    Kind = "entity_facts"
)

// Descriptor declares a derived view's public identity.
// Version is the view schema/contract version exposed by this package; it is
// not the transform recipe hash that produced a specific output.
type Descriptor struct {
	ID      ID
	Kind    Kind
	Version string
}

// View is the minimal contract every derived view exposes.
type View interface {
	Descriptor() Descriptor
}

// Validate checks that the descriptor is specific enough to register.
func (d Descriptor) Validate() error {
	if d.ID == "" {
		return errdefs.Validationf("memory/views: view id is required")
	}
	if !validViewKind(d.Kind) {
		return errdefs.Validationf("memory/views: unsupported view kind %q", d.Kind)
	}
	if d.Version == "" {
		return errdefs.Validationf("memory/views: view version is required")
	}
	return nil
}

func validViewKind(kind Kind) bool {
	switch kind {
	case KindRecentWindow,
		KindSummaryDAG,
		KindDocumentChunks,
		KindMessageIndex,
		KindEntityFacts:
		return true
	default:
		return false
	}
}
