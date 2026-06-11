package fact

import (
	"maps"
	"time"

	"github.com/GizClaw/flowcraft/memory/views"
)

// FactID is a stable fact identifier in the ledger.
type FactID string

// FactStatus describes the lifecycle state of a reconciled fact.
type FactStatus string

const (
	FactActive     FactStatus = "active"
	FactSuperseded FactStatus = "superseded"
	FactRetracted  FactStatus = "retracted"
)

// ObservationRef points at the observation ledger record that contributed to a
// reconciled fact.
type ObservationRef struct {
	ObservationID string
	ScopeKind     string
	ScopeID       string
}

// Fact is one longer-lived claim reconciled from grounded observations.
//
// SourceRefs are denormalized canonical evidence refs for citation and
// hydration. Signature records the observation ledger outputs used to derive the
// fact, and may also carry canonical source revisions when a recipe chooses to
// include them.
type Fact struct {
	ID              FactID
	Scope           views.Scope
	Subject         string
	Predicate       string
	Object          string
	Status          FactStatus
	Confidence      float64
	ValidFrom       *time.Time
	ValidUntil      *time.Time
	ObservationRefs []ObservationRef
	SourceRefs      []views.SourceRef
	Signature       views.ViewSignature
	CreatedAt       time.Time
	UpdatedAt       time.Time
	// Metadata must be JSON-compatible. map[string]any and []any values are
	// deep-cloned across facade/store boundaries.
	Metadata map[string]any
}

func normalizeFact(fact Fact) Fact {
	if fact.Status == "" {
		fact.Status = FactActive
	}
	return fact
}

func cloneFact(in Fact) Fact {
	out := in
	out.ValidFrom = cloneTimePtr(in.ValidFrom)
	out.ValidUntil = cloneTimePtr(in.ValidUntil)
	out.ObservationRefs = cloneObservationRefs(in.ObservationRefs)
	out.SourceRefs = cloneSourceRefs(in.SourceRefs)
	out.Signature = cloneViewSignature(in.Signature)
	if in.Metadata != nil {
		out.Metadata = cloneAnyMap(in.Metadata)
	}
	return out
}

func cloneFacts(in []Fact) []Fact {
	if in == nil {
		return nil
	}
	out := make([]Fact, len(in))
	for i, fact := range in {
		out[i] = cloneFact(fact)
	}
	return out
}

func cloneTimePtr(in *time.Time) *time.Time {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneObservationRefs(in []ObservationRef) []ObservationRef {
	if in == nil {
		return nil
	}
	return append([]ObservationRef(nil), in...)
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
