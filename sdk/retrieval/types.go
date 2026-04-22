package retrieval

import "time"

// Doc is the minimal unit indexed under a namespace.
type Doc struct {
	ID       string
	Content  string
	Vector   []float32
	Metadata map[string]any

	SparseVector map[string]float32
	Timestamp    time.Time
}

// Range is an inclusive/exclusive bound for metadata numeric comparisons.
type Range struct {
	Gt, Gte, Lt, Lte any
}

// Filter is a structured predicate tree over Doc.Metadata (flat keys only).
type Filter struct {
	And []Filter
	Or  []Filter
	Not *Filter

	Eq      map[string]any
	Neq     map[string]any
	In      map[string][]any
	NotIn   map[string][]any
	Range   map[string]Range
	Exists  []string
	Missing []string
	Match   map[string]string

	Contains    map[string]any
	IContains   map[string]any
	ContainsAny map[string][]any
	ContainsAll map[string][]any
}
