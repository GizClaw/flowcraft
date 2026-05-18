package retrieval

// Capabilities describes what an Index implementation supports natively.
type Capabilities struct {
	BM25   bool
	Vector bool
	Sparse bool
	Hybrid bool

	FilterPushdown bool
	MaxFilterDepth int
	SupportedOps   []string

	Rerank bool

	BatchUpsertMax int
	WriteIsAtomic  bool

	MaxListPageSize      int
	NativeDeleteByFilter bool
	SupportedListOrders  []ListOrderBy

	ReadAfterWrite bool
	Distributed    bool

	// Debug reports whether the backend will honour SearchRequest.Debug
	// (or HybridRequest.Debug) by populating SearchResponse.Execution.
	//
	// Backends that delegate retrieval to a higher-level pipeline (e.g.
	// MemoryIndex used through retrieval/pipeline) typically leave this
	// false: pipelines populate Execution themselves; the backend has no
	// view of lanes/stages on the direct Search path.
	Debug bool

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
	HybridSearch   bool
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
		Sparse: false,
		Hybrid: true,

		FilterPushdown: true,
		MaxFilterDepth: -1,
		SupportedOps: []string{
			"eq", "neq", "in", "nin", "range", "exists", "missing",
			"contains", "icontains", "contains_any", "contains_all",
			"and", "or", "not", "match",
		},

		Rerank: false,

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
	c.Extensions = extensionCapabilitiesOf(idx, c.Extensions)
	if c.Hybrid && !c.Extensions.HybridSearch {
		c.Hybrid = false
	}
	if c.NativeDeleteByFilter && !c.Extensions.DeleteByFilter {
		c.NativeDeleteByFilter = false
	}
	return c
}

func extensionCapabilitiesOf(idx Index, declared ExtensionCapabilities) ExtensionCapabilities {
	if _, ok := idx.(DocGetter); ok {
		declared.DocGetter = true
	}
	if _, ok := idx.(Filterable); ok {
		declared.Filterable = true
	}
	if _, ok := idx.(Hybridable); ok {
		declared.HybridSearch = true
	}
	if _, ok := idx.(Vectorizable); ok {
		declared.Vectorizable = true
	}
	if _, ok := idx.(Snapshottable); ok {
		declared.Snapshottable = true
	}
	if _, ok := idx.(Iterable); ok {
		declared.Iterable = true
	}
	if _, ok := idx.(Countable); ok {
		declared.Count = true
	}
	if _, ok := idx.(DeletableByFilter); ok {
		declared.DeleteByFilter = true
	}
	if _, ok := idx.(Droppable); ok {
		declared.DropNamespace = true
	}
	return declared
}
