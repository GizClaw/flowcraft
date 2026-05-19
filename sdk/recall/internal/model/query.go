package model

type QueryIntent struct {
	Text     string
	Entities []string
	Kinds    []FactKind
}

type QueryPlan struct {
	Intent        QueryIntent
	SourceOrder   []string
	SourceBudgets map[string]int
}

type Candidate struct {
	FactID string
	Scope  Scope
	Source string
	Rank   int
	Score  float64

	EvidenceIDs []string
	Metadata    map[string]any
}

type SourceResult struct {
	Source     string
	Candidates []Candidate
	Truncated  bool
	Err        error
}
