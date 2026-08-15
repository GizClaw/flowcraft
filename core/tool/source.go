package tool

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

// Source is a multi-tool contributor to a [Registry]: builtin Go
// functions, a sandbox-backed exec set, an MCP server, a YAML
// document, a host-provided collection. Sources are resources in the
// deployment DAG; a registry aggregates them through a Many dep.
//
// Tools() are eager — available immediately without I/O.
// LazyTools() are deferred: the registry stores a proxy serving
// [LazyTool.Placeholder] as its definition, and the real
// implementation is loaded on first Execute (never at build time).
type Source interface {
	Tools() []Tool
	LazyTools() []LazyTool
}

// Registrar is the runtime mutation surface of a [Registry]. Sources
// that discover tools after construction — an MCP server that connects
// in the background, a refresh that re-lists remote tools — use it to
// publish tools as they become available.
type Registrar interface {
	// Add registers one tool. Duplicate names follow the registry's
	// ConflictPolicy; adding to a closed registry returns an error.
	Add(t Tool) error
	// Remove unregisters the named tool, if present.
	Remove(name string)
}

// RegistryAttacher is an optional [Source] capability: the assembly
// hands the built [Registry]'s [Registrar] to sources that need to
// publish tools after construction (background connects, refresh).
type RegistryAttacher interface {
	Attach(r Registrar)
}

// LazyTool is a deferred tool descriptor contributed by a Source.
// Name must equal Placeholder.Name; Load performs any network or
// process work the tool needs.
type LazyTool struct {
	Name        string
	Placeholder message.ToolDefinition
	Load        func(ctx context.Context) (Tool, error)
}

// Validate checks the descriptor invariants.
func (lt LazyTool) Validate() error {
	if strings.TrimSpace(lt.Name) == "" {
		return errdefs.Validationf("tool: lazy tool name is required")
	}
	if lt.Placeholder.Name != lt.Name {
		return errdefs.Validationf(
			"tool: lazy tool %q placeholder must declare the same name", lt.Name)
	}
	if lt.Load == nil {
		return errdefs.Validationf("tool: lazy tool %q has nil loader", lt.Name)
	}
	return nil
}
