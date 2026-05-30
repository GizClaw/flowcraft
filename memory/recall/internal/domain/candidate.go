package domain

import "time"

// Candidate is the unit emitted by every CandidateSource. It is a
// pure pointer to a canonical fact + provenance: sources never
// materialize the fact itself (docs §9.2). EvidenceIDs survive into
// materialization so trace explanations can attribute hits.
type Candidate struct {
	FactID string
	Scope  Scope
	Source string
	Rank   int
	Score  float64

	EvidenceIDs []string
	Metadata    map[string]any
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
