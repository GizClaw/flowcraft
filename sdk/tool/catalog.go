package tool

// Catalog is the read-side view of a tool collection: lookup plus
// model-facing definitions. Executors dispatch against a Catalog, so
// any source — the mutable Registry, a filtered view, a remote proxy —
// can back execution without implementing dispatch policy itself.
type Catalog interface {
	// Get returns the tool registered under name.
	Get(name string) (Tool, bool)
	// Definitions returns the Definition of every tool in the catalog.
	Definitions() []Definition
}
