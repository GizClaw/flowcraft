package observation

import (
	"maps"
	"time"

	"github.com/GizClaw/flowcraft/memory/views"
)

// Scope identifies the lightweight domain boundary an observation belongs to.
//
// Kind and ID are the stable scope identity. Optional fields let callers carry
// common projections without depending on source packages.
type Scope struct {
	Kind           string
	ID             string
	DatasetID      string
	ConversationID string
	EntityID       string
}

// Observation is one derived semantic observation backed by canonical evidence.
//
// Observations are not canonical evidence themselves. SourceRefs cite the
// evidence spans used to derive the statement, and Signature records the source
// revisions used for freshness checks.
type Observation struct {
	ID         string
	Scope      Scope
	Subject    string
	Predicate  string
	Object     string
	Confidence float64
	SourceRefs []views.SourceRef
	Signature  views.ViewSignature
	CreatedAt  time.Time
	UpdatedAt  time.Time
	// Metadata must be JSON-compatible. map[string]any and []any values are
	// deep-cloned across facade/store boundaries.
	Metadata map[string]any
}

func cloneObservation(in Observation) Observation {
	out := in
	out.SourceRefs = cloneSourceRefs(in.SourceRefs)
	out.Signature = cloneViewSignature(in.Signature)
	if in.Metadata != nil {
		out.Metadata = cloneAnyMap(in.Metadata)
	}
	return out
}

func cloneObservations(in []Observation) []Observation {
	if in == nil {
		return nil
	}
	out := make([]Observation, len(in))
	for i, observation := range in {
		out[i] = cloneObservation(observation)
	}
	return out
}

func cloneAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneAny(v)
	}
	return out
}

func cloneAny(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return cloneAnyMap(val)
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = cloneAny(item)
		}
		return out
	default:
		return v
	}
}

func cloneSourceRefs(in []views.SourceRef) []views.SourceRef {
	if in == nil {
		return nil
	}
	out := make([]views.SourceRef, len(in))
	for i, ref := range in {
		out[i] = cloneSourceRef(ref)
	}
	return out
}

func cloneSourceRef(ref views.SourceRef) views.SourceRef {
	if ref.Message != nil {
		msg := *ref.Message
		if msg.Span != nil {
			span := *msg.Span
			msg.Span = &span
		}
		ref.Message = &msg
	}
	if ref.Document != nil {
		doc := *ref.Document
		if doc.Span != nil {
			span := *doc.Span
			doc.Span = &span
		}
		ref.Document = &doc
	}
	return ref
}

func cloneViewSignature(in views.ViewSignature) views.ViewSignature {
	out := in
	if in.SourceRevisions != nil {
		out.SourceRevisions = append([]views.SourceRevision(nil), in.SourceRevisions...)
	}
	if in.UpstreamViewRefs != nil {
		out.UpstreamViewRefs = append([]views.UpstreamViewRef(nil), in.UpstreamViewRefs...)
	}
	if in.DiagnosticSignatures != nil {
		out.DiagnosticSignatures = maps.Clone(in.DiagnosticSignatures)
	}
	return out
}
