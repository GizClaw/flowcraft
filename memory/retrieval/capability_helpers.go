package retrieval

// Capability names a backend feature at the public retrieval boundary.
type Capability string

const (
	CapabilityBM25                 Capability = "bm25"
	CapabilityVector               Capability = "vector"
	CapabilitySparse               Capability = "sparse"
	CapabilityHybrid               Capability = "hybrid"
	CapabilityFilterPushdown       Capability = "filter_pushdown"
	CapabilityDebug                Capability = "debug"
	CapabilityDocGetter            Capability = "doc_getter"
	CapabilityIterable             Capability = "iterable"
	CapabilityCount                Capability = "count"
	CapabilityDeleteByFilter       Capability = "delete_by_filter"
	CapabilityDropNamespace        Capability = "drop_namespace"
	CapabilitySnapshot             Capability = "snapshot"
	CapabilityVectorizable         Capability = "vectorizable"
	CapabilityNativeDeleteByFilter Capability = "native_delete_by_filter"
)

// Supports centralises capability checks so callers do not spread
// Capabilities + type-assertion combinations across the codebase.
func Supports(idx Index, cap Capability) bool {
	if idx == nil {
		return false
	}
	c := CapabilitiesOf(idx)
	switch cap {
	case CapabilityBM25:
		return c.BM25
	case CapabilityVector:
		return c.Vector
	case CapabilitySparse:
		return c.Sparse
	case CapabilityHybrid:
		return c.Hybrid
	case CapabilityFilterPushdown:
		return c.FilterPushdown
	case CapabilityDebug:
		return c.Debug
	case CapabilityDocGetter:
		return c.Extensions.DocGetter
	case CapabilityIterable:
		return c.Extensions.Iterable
	case CapabilityCount:
		return c.Extensions.Count
	case CapabilityDeleteByFilter:
		return c.Extensions.DeleteByFilter
	case CapabilityNativeDeleteByFilter:
		return c.NativeDeleteByFilter
	case CapabilityDropNamespace:
		return c.Extensions.DropNamespace
	case CapabilitySnapshot:
		return c.Extensions.Snapshottable
	case CapabilityVectorizable:
		return c.Extensions.Vectorizable
	default:
		return false
	}
}

func AsDocGetter(idx Index) (DocGetter, bool) {
	if !Supports(idx, CapabilityDocGetter) {
		return nil, false
	}
	g, ok := idx.(DocGetter)
	return g, ok
}

func AsHybrid(idx Index) (Hybridable, bool) {
	if !Supports(idx, CapabilityHybrid) {
		return nil, false
	}
	h, ok := idx.(Hybridable)
	return h, ok
}

func AsIterable(idx Index) (Iterable, bool) {
	if !Supports(idx, CapabilityIterable) {
		return nil, false
	}
	it, ok := idx.(Iterable)
	return it, ok
}

func AsCountable(idx Index) (Countable, bool) {
	if !Supports(idx, CapabilityCount) {
		return nil, false
	}
	c, ok := idx.(Countable)
	return c, ok
}

func AsDeletableByFilter(idx Index) (DeletableByFilter, bool) {
	if !Supports(idx, CapabilityDeleteByFilter) {
		return nil, false
	}
	d, ok := idx.(DeletableByFilter)
	return d, ok
}

func AsDroppable(idx Index) (Droppable, bool) {
	if !Supports(idx, CapabilityDropNamespace) {
		return nil, false
	}
	d, ok := idx.(Droppable)
	return d, ok
}
