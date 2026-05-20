package domain

// QueryPlan describes how the read pipeline will visit candidate
// sources for a single Recall call.
type QueryPlan struct {
	Intent        QueryIntent
	SourceOrder   []string
	SourceBudgets map[string]int
	TotalCap      int
}
