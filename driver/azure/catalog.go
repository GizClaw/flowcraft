package azure

// catalogEntry is one deployment's compile-time capability declaration.
// Azure routes by deployment name, so the spec's models list is the whole
// catalog: the factory maps each declared deployment onto an entry, and the
// compiler rejects every channel the entry omits.
type catalogEntry struct {
	kind       modelKind
	vision     bool
	reasoning  bool
	webSearch  bool
	dimensions bool
}

// entryFor lowers one declared deployment into a compiler entry.
func entryFor(model ModelSpec) catalogEntry {
	return catalogEntry{
		kind:       modelKind(model.Kind),
		vision:     model.Vision,
		reasoning:  model.Reasoning,
		webSearch:  model.WebSearch,
		dimensions: model.Dimensions,
	}
}
