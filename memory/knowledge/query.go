package knowledge

// Scope expresses the dataset breadth of a query.
type Scope int

const (
	// ScopeSingleDataset restricts the search to Query.DatasetID.
	ScopeSingleDataset Scope = iota
	// ScopeAllDatasets searches across every known dataset.
	ScopeAllDatasets
)

// Mode is an alias for SearchMode, kept so newer call-sites can read
// "knowledge.Mode" without the redundant Search prefix while older
// "knowledge.SearchMode" call sites keep working unchanged.
type Mode = SearchMode

// IsValidMode reports whether m is a recognised mode.
//
// The empty string is accepted for backwards compatibility (legacy
// callers used "" to mean BM25); ResolveMode normalises it to ModeBM25.
func IsValidMode(m Mode) bool {
	switch m {
	case ModeBM25, ModeVector, ModeHybrid, "":
		return true
	}
	return false
}

// ResolveMode normalises zero values to a canonical Mode.
//
//   - ""           -> ModeBM25 (legacy default)
//
// Any other recognised mode is returned unchanged.
func ResolveMode(m Mode) Mode {
	if m == "" {
		return ModeBM25
	}
	return m
}

// Query is the canonical search input.
//
// Validation rules (enforced by Service.Search):
//   - Layer must be valid; defaults to LayerDetail when zero.
//   - Mode  must be valid; defaults to ModeBM25 when zero.
//   - Scope=ScopeSingleDataset requires DatasetID to be non-empty.
type Query struct {
	DatasetID string
	Scope     Scope
	Text      string
	Mode      Mode
	Layer     Layer
	TopK      int
	Threshold float64

	// resolvedDatasets is set by Service.Search after resolving
	// ScopeAllDatasets via DocumentRepo.ListDatasets, so retrievers can
	// fan out without re-enumerating datasets per lane. Unexported on
	// purpose: callers MUST go through Service.Search.
	resolvedDatasets []string
}

// withDatasets returns a copy of q whose resolvedDatasets is set; used
// by Service.Search to push the resolved fan-out list down to
// retrievers without mutating the caller's Query.
func (q Query) withDatasets(ids []string) Query {
	q.resolvedDatasets = ids
	return q
}

// Hit is one ranked search result.
type Hit struct {
	DatasetID  string
	DocName    string
	Layer      Layer
	Content    string
	Score      float64
	ChunkIndex int            // -1 for layer hits
	Sig        DerivedSig     // freshness traceability
	Metadata   map[string]any // backend-passthrough metadata
}

// Result wraps the ordered hit list returned by Service.Search.
type Result struct {
	Hits []Hit
}
