package tool

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// RegistryKind is the deployment resource kind of a tool registry.
const RegistryKind = "tool.Registry"

// depToolSources is the tool source set an assembly or registry is built
// from: every dep key equal to "tool" or starting with "tool.".
var depToolSources = resource.RequireMany[Source]("tool")

// RegistryFactory builds a memory registry from its many tool.Source
// deps. The dep order (GetAll's sorted key order) is the explicit source
// order used by ConflictOverwrite.
type RegistryFactory struct{}

// Spec implements resource.Factory.
func (RegistryFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: RegistryKind,
		Impl: "memory",
		Deps: []resource.Requirement{depToolSources.Spec()},
	}
}

// New implements resource.Factory.
func (RegistryFactory) New(_ context.Context, in resource.Input) (*Registry, error) {
	sources, err := depToolSources.GetAll(in)
	if err != nil {
		return nil, err
	}
	return NewRegistry(sources)
}

// Register adds the memory registry factory to r.
func Register(r *resource.Registry) error {
	if err := resource.Register(r, RegistryFactory{}); err != nil {
		return err
	}
	return resource.Register(r, AssemblyFactory{})
}

// AssemblyKind is the deployment resource kind of the standard tool
// assembly (registry + executor + optional dynamic injection).
const AssemblyKind = "tool.Assembly"

// AssemblySettings is the strict settings subtree of the memory
// assembly factory: the dynamic injection policy only. Middleware
// settings belong to the tool.Assembly/middleware impl (see
// core/tool/middleware); the memory impl rejects the "middlewares"
// key via strict decoding.
type AssemblySettings struct {
	Dynamic *Policy `json:"dynamic,omitempty"`
}

// AssemblyFactory builds the standard tool assembly resource without
// middleware: the executor runs a bare chain. Deployments that need
// middleware use the tool.Assembly/middleware impl.
type AssemblyFactory struct{}

// Spec implements resource.Factory.
func (AssemblyFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: AssemblyKind,
		Impl: "memory",
		Deps: []resource.Requirement{depToolSources.Spec()},
	}
}

// New implements resource.Factory.
func (AssemblyFactory) New(ctx context.Context, in resource.Input) (*Assembly, error) {
	settings, err := resource.DecodeTyped[AssemblySettings](ctx, in.Settings)
	if err != nil {
		return nil, errdefs.Validationf(
			"tool: decode assembly settings: %w", err)
	}

	sources, err := depToolSources.GetAll(in)
	if err != nil {
		return nil, err
	}

	var opts []AssemblyOption
	if settings.Dynamic != nil {
		opts = append(opts, WithDynamic(*settings.Dynamic))
	}
	return NewAssembly(sources, opts...)
}
