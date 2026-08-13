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
	Definitions() []message.ToolDefinition
}
