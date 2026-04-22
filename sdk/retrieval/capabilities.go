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
	}
}
