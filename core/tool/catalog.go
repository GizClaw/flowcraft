package tool

import "github.com/GizClaw/flowcraft/core/message"

// Catalog is the read-side view of a tool collection: lookup plus
// model-facing definitions. A [Registry] implements it; read-side
// wrappers (e.g. the dynamic injection catalog) implement it over a
// base registry without being execution surfaces.
type Catalog interface {
	// Get returns the tool registered under name.
	Get(name string) (Tool, bool)
	// Definitions returns the Definition of every tool in the catalog.
	//
	// A Registry with a decorator ([WithDefinitionDecorator]) returns the decorated
	// text, which is what the model reads: a reader that re-derives the list from
	// Get alone would silently undo a deployment's overrides.
	Definitions() []message.ToolDefinition
}
