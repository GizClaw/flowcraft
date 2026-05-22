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
//
// Each bound is interpreted as float64; mixed numeric kinds in metadata
// (int / int64 / float32 / float64 / uint64) are normalised to float64
// before comparison so callers do not need to align the wire type.
type Range struct {
	Gt, Gte, Lt, Lte any
}

// Filter is a structured predicate tree over Doc.Metadata (flat keys only).
//
// All metadata keys reference Doc.Metadata directly except for "_content",
// which is a reserved key that maps to Doc.Content for the Match family —
// useful when callers want a substring filter over the document body
// without round-tripping through metadata.
//
// Composition: empty maps / nil branches are no-ops, so a zero Filter
// matches every document. Backends that cannot push down a particular
// operator should return Filterable.SupportsFilter=false and let
// pipeline.PostFilter (or DocMatchesFilter) handle it client-side.
type Filter struct {
	// And requires every sub-filter to match.
	And []Filter
	// Or matches if any sub-filter matches; an empty Or slice is ignored
	// so it cannot be confused with the always-false predicate.
	Or []Filter
	// Not negates the sub-filter; combined with sibling predicates via
	// implicit AND (see filter_match_test for the exact semantics).
	Not *Filter

	// Eq requires Doc.Metadata[k] to equal v after numeric normalisation.
	Eq map[string]any
	// Neq is the negation of Eq.
	Neq map[string]any
	// In requires Doc.Metadata[k] to equal at least one element of the
	// list (set membership).
	In map[string][]any
	// NotIn is the negation of In.
	NotIn map[string][]any
	// Range applies numeric Gt/Gte/Lt/Lte bounds to Doc.Metadata[k].
	Range map[string]Range
	// Exists requires Doc.Metadata[k] to be present (any value).
	Exists []string
	// Missing requires Doc.Metadata[k] to be absent.
	Missing []string
	// Match requires the value at k (or Doc.Content when k=="_content") to
	// contain the given substring. Case-sensitive; for case-insensitive
	// matches use IContains.
	Match map[string]string

	// Contains requires Doc.Metadata[k] (string or array) to contain v
	// case-sensitively. Strings use substring; arrays use element equality.
	Contains map[string]any
	// IContains is the case-insensitive variant of Contains.
	IContains map[string]any
	// ContainsAny matches when Doc.Metadata[k] (array) shares at least one
	// element with the supplied list.
	ContainsAny map[string][]any
	// ContainsAll matches when Doc.Metadata[k] (array) contains every
	// element of the supplied list.
	ContainsAll map[string][]any
}
