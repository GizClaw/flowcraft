package domain

import "time"

// CandidateRef is the unit emitted by every read Source. It is a pure pointer
// to one canonical graph node plus read-time provenance: sources never
// materialize the node itself. Assertion sources emit GraphNodeAssertion refs,
// observation sources emit GraphNodeObservation refs, and future link sources
// can emit link refs without pretending everything is a TemporalFact.
type CandidateRef struct {
	Kind GraphNodeKind
	ID   string

	Scope  Scope
	Source string
	Rank   int
	Score  float64

	EvidenceIDs      []string
	DiscoverySignals []DiscoverySignal
	Metadata         map[string]any
}

// Candidate is kept as the read pipeline vocabulary while its schema is now a
// canonical graph-node ref rather than a fact-only pointer.
type Candidate = CandidateRef

func NewAssertionCandidate(id string, scope Scope, source string, score float64) Candidate {
	return Candidate{Kind: GraphNodeAssertion, ID: id, Scope: scope, Source: source, Score: score}
}

func NewObservationCandidate(id string, scope Scope, source string, score float64) Candidate {
	return Candidate{Kind: GraphNodeObservation, ID: id, Scope: scope, Source: source, Score: score}
}

func NewLinkCandidate(id string, scope Scope, source string, score float64) Candidate {
	return Candidate{Kind: GraphNodeLink, ID: id, Scope: scope, Source: source, Score: score}
}

func (c CandidateRef) Node() GraphNodeRef {
	return GraphNodeRef{Kind: c.Kind, ID: c.ID}
}

func (c CandidateRef) AssertionID() string {
	if c.Kind == "" || c.Kind == GraphNodeAssertion {
		return c.ID
	}
	return ""
}

// SourceResult is one source's contribution to a query. Truncated
// signals the source hit its budget; Err carries non-fatal source
// failures so the fusion layer can degrade rather than abort.
type SourceResult struct {
	Source     string
	Candidates []Candidate
	Truncated  bool
	Err        error
	Latency    time.Duration
}
