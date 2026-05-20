package domain

// ContextItem is a materialized recall result. The Candidate field
// preserves the fusion provenance (score, source, rank) so explain
// traces and future ranking layers can use it.
type ContextItem struct {
	Candidate Candidate
	Fact      TemporalFact
	Evidence  []EvidenceRef
}
