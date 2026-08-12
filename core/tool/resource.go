package tool

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// RegistryKind is the deployment resource kind of a tool registry.
const RegistryKind = "tool.Registry"

// RegistryFactory builds a memory registry from its many tool.Source
// deps. The dep order (DepsMany's sorted key order) is the explicit
// source order used by ConflictOverwrite.
type RegistryFactory struct{}

// Spec implements resource.Factory.
func (RegistryFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: RegistryKind,
		Impl: "memory",
		Deps: []resource.DepSpec{{
			Name: "tool", Type: "tool.Source", Required: true, Many: true,
		}},
	}
}

// New implements resource.Factory.
func (RegistryFactory) New(_ context.Context, in resource.Input) (any, error) {
	values := in.DepsMany("tool")
	sources := make([]Source, 0, len(values))
	for _, value := range values {
		src, ok := value.(Source)
		if !ok {
			return nil, errdefs.Validationf(
				"tool: registry dep is %T, want tool.Source", value)
		}
		sources = append(sources, src)
	}
	if len(sources) == 0 {
		return nil, errdefs.Validationf(
			"tool: registry requires at least one tool source")
	}
	return NewRegistry(sources)
}

// Register adds the memory registry factory to r.
func Register(r *resource.Registry) error {
	if err := r.Register(RegistryFactory{}); err != nil {
		return err
	}
	return r.Register(AssemblyFactory{})
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
		Deps: []resource.DepSpec{{
			Name: "tool", Type: "tool.Source", Required: true, Many: true,
		}},
	}
}

// New implements resource.Factory.
func (AssemblyFactory) New(_ context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[AssemblySettings](in.Settings)
	if err != nil {
		return nil, errdefs.Validationf(
			"tool: decode assembly settings: %v", err)
	}

	values := in.DepsMany("tool")
	sources := make([]Source, 0, len(values))
	for _, value := range values {
		src, ok := value.(Source)
		if !ok {
			return nil, errdefs.Validationf(
				"tool: assembly dep is %T, want tool.Source", value)
		}
		sources = append(sources, src)
	}
	if len(sources) == 0 {
		return nil, errdefs.Validationf(
			"tool: assembly requires at least one tool source")
	}

	var opts []AssemblyOption
	if settings.Dynamic != nil {
		opts = append(opts, WithDynamic(*settings.Dynamic))
	}
	return NewAssembly(sources, opts...)
}
