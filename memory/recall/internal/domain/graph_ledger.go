package domain

import "time"

// ObservationKind classifies raw evidence captured before assertion extraction.
type ObservationKind string

const (
	ObservationKindTurn     ObservationKind = "turn"
	ObservationKindEvidence ObservationKind = "evidence"
	ObservationKindDocument ObservationKind = "document"
)

// ObservationSpanKind classifies an addressable span inside an Observation.
type ObservationSpanKind string

const (
	ObservationSpanKindText  ObservationSpanKind = "text"
	ObservationSpanKindQuote ObservationSpanKind = "quote"
)

// ObservationSpan is an addressable raw-evidence slice. It lets links and
// EvidenceRefs point at the exact supporting span without splitting the parent
// Observation source object into many unrelated nodes.
type ObservationSpan struct {
	ID            string
	ObservationID string
	SourceID      string
	Kind          ObservationSpanKind
	Text          string
	Start         int
	End           int
	Metadata      map[string]any
}

// Clone returns a defensive copy of the span.
func (s ObservationSpan) Clone() ObservationSpan {
	out := s
	out.Metadata = cloneMetadata(s.Metadata)
	return out
}

// Observation is the append-only raw evidence boundary of the memory graph.
// Assertions may be re-extracted or superseded, but observations preserve the
// source material that caused those assertions to exist.
type Observation struct {
	ID    string
	Scope Scope
	Kind  ObservationKind

	SourceID  string
	SessionID string
	MessageID string
	Role      string
	Speaker   string
	Text      string

	Spans []ObservationSpan

	ObservedAt time.Time
	ReceivedAt time.Time

	Metadata map[string]any
}

// Clone returns a defensive copy of the observation.
func (o Observation) Clone() Observation {
	out := o
	if len(o.Spans) > 0 {
		out.Spans = make([]ObservationSpan, 0, len(o.Spans))
		for _, span := range o.Spans {
			out.Spans = append(out.Spans, span.Clone())
		}
	}
	out.Metadata = cloneMetadata(o.Metadata)
	return out
}

// GraphNodeKind identifies the canonical node namespace used by FactLink.
type GraphNodeKind string

const (
	GraphNodeObservation     GraphNodeKind = "observation"
	GraphNodeObservationSpan GraphNodeKind = "observation_span"
	GraphNodeAssertion       GraphNodeKind = "assertion"
	GraphNodeLink            GraphNodeKind = "link"
)

// GraphNodeRef points at one canonical graph node.
type GraphNodeRef struct {
	Kind GraphNodeKind
	ID   string
}

// FactLinkType classifies first-class relationships in the canonical graph.
type FactLinkType string

const (
	// LinkDerivedFrom means the assertion was derived from the observation.
	LinkDerivedFrom FactLinkType = "derived_from"
	// LinkSupports means the observation provides evidence for the assertion.
	LinkSupports FactLinkType = "supports"
	// LinkSupersedes means one assertion replaces a prior assertion.
	LinkSupersedes FactLinkType = "supersedes"
)

// FactLink is a typed edge in the canonical memory graph.
type FactLink struct {
	ID    string
	Scope Scope
	Type  FactLinkType

	From GraphNodeRef
	To   GraphNodeRef

	MergeKey   string
	Confidence float64
	CreatedAt  time.Time

	EvidenceObservationIDs []string
	EvidenceRefs           []EvidenceRef
	Metadata               map[string]any
}

// Clone returns a defensive copy of the link.
func (l FactLink) Clone() FactLink {
	out := l
	out.EvidenceObservationIDs = cloneStrings(l.EvidenceObservationIDs)
	out.EvidenceRefs = cloneEvidence(l.EvidenceRefs)
	out.Metadata = cloneMetadata(l.Metadata)
	return out
}

// MemoryGraphDelta is the write unit for the experimental graph ledger. It
// mirrors the existing Save resolution shape while making raw observations and
// typed links explicit.
type MemoryGraphDelta struct {
	Observations []Observation
	Assertions   []TemporalFact
	Links        []FactLink
	Closes       []ValidityClose
}

// Clone returns a defensive copy of the graph delta.
func (d MemoryGraphDelta) Clone() MemoryGraphDelta {
	out := MemoryGraphDelta{
		Observations: make([]Observation, 0, len(d.Observations)),
		Assertions:   make([]TemporalFact, 0, len(d.Assertions)),
		Links:        make([]FactLink, 0, len(d.Links)),
		Closes:       append([]ValidityClose(nil), d.Closes...),
	}
	for _, o := range d.Observations {
		out.Observations = append(out.Observations, o.Clone())
	}
	for _, f := range d.Assertions {
		out.Assertions = append(out.Assertions, f.Clone())
	}
	for _, l := range d.Links {
		out.Links = append(out.Links, l.Clone())
	}
	return out
}
