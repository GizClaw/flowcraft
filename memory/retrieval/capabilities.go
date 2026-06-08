package retrieval

// Capabilities describes the semantics an Index implementation supports
// through Index.Search and the optional management interfaces below.
type Capabilities struct {
	// BM25 means QueryText participates in native ranking.
	BM25 bool
	// Vector means QueryVector participates in native ranking.
	Vector bool
	// Sparse means SparseVec participates in native ranking.
	Sparse bool
	// Hybrid means one Index.Search call can combine two or more supported
	// query signals (QueryText, QueryVector, SparseVec) using HybridMode and
	// HybridOptions.
	// It does not imply a separate optional hybrid execution interface.
	Hybrid bool

	// FilterPushdown means Search/List/Count/DeleteByFilter evaluate Filter in
	// the backend's own execution path rather than requiring caller-side scans.
	FilterPushdown bool
	MaxFilterDepth int
	SupportedOps   []string

	BatchUpsertMax int
	WriteIsAtomic  bool

	MaxListPageSize int
	// NativeDeleteByFilter means the backend can delete by predicate in its
	// own storage path; scan/list + Delete fallbacks must report false.
	NativeDeleteByFilter bool
	SupportedListOrders  []ListOrderBy

	ReadAfterWrite bool
	Distributed    bool

	// Extensions declares optional retrieval.Index extension interfaces
	// implemented by this backend. Callers should prefer CapabilitiesOf(idx)
	// or Supports(idx, ...) over reading this field directly so wrappers can
	// project the declared surface against their actual method set.
	Extensions ExtensionCapabilities
}

// ExtensionCapabilities describes optional execution interfaces. Capabilities
// is the declaration surface; type assertions are only the execution path.
type ExtensionCapabilities struct {
	DocGetter      bool
	Filterable     bool
	Vectorizable   bool
	Snapshottable  bool
	Iterable       bool
	Count          bool
	DeleteByFilter bool
	DropNamespace  bool
}

// DefaultMemoryCapabilities returns capabilities for MemoryIndex.
func DefaultMemoryCapabilities() Capabilities {
	return Capabilities{
		BM25:   true,
		Vector: true,
		Sparse: true,
		Hybrid: true,

		FilterPushdown: true,
		MaxFilterDepth: -1,
		SupportedOps: []string{
			"eq", "neq", "in", "nin", "range", "exists", "missing",
			"contains", "icontains", "contains_any", "contains_all",
			"and", "or", "not", "match",
		},

		BatchUpsertMax: 0,
		WriteIsAtomic:  true,

		MaxListPageSize:      10_000,
		NativeDeleteByFilter: true,
		SupportedListOrders:  []ListOrderBy{OrderByTimestampDesc, OrderByTimestampAsc, OrderByIDAsc},

		ReadAfterWrite: true,
		Distributed:    false,
		Extensions: ExtensionCapabilities{
			DocGetter:      true,
			Iterable:       true,
			Count:          true,
			DeleteByFilter: true,
			DropNamespace:  true,
		},
	}
}

// CapabilitiesOf returns idx.Capabilities normalized against idx's actual
// optional method set. It is the preferred capability read path for callers.
func CapabilitiesOf(idx Index) Capabilities {
	if idx == nil {
		return Capabilities{}
	}
	c := idx.Capabilities()
	c.Extensions = extensionCapabilitiesOf(idx)
	if c.NativeDeleteByFilter && !c.Extensions.DeleteByFilter {
		c.NativeDeleteByFilter = false
	}
	return c
}

func extensionCapabilitiesOf(idx Index) ExtensionCapabilities {
	var projected ExtensionCapabilities
	if _, ok := idx.(DocGetter); ok {
		projected.DocGetter = true
	}
	if _, ok := idx.(Filterable); ok {
		projected.Filterable = true
	}
	if _, ok := idx.(Vectorizable); ok {
		projected.Vectorizable = true
	}
	if _, ok := idx.(Snapshottable); ok {
		projected.Snapshottable = true
	}
	if _, ok := idx.(Iterable); ok {
		projected.Iterable = true
	}
	if _, ok := idx.(Countable); ok {
		projected.Count = true
	}
	if _, ok := idx.(DeletableByFilter); ok {
		projected.DeleteByFilter = true
	}
	if _, ok := idx.(Droppable); ok {
		projected.DropNamespace = true
	}
	return projected
}
