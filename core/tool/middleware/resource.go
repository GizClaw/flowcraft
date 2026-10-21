package middleware

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/tool"
)

// AssemblyImpl is the impl name of the middleware-enabled tool
// assembly.
const AssemblyImpl = "middleware"

// depToolSources is the tool source set this assembly is built from.
var depToolSources = resource.RequireMany[tool.Source]("tool")

// AssemblySettings is the strict settings subtree of the
// tool.Assembly/middleware factory: the standard middleware chain plus
// the dynamic injection policy. The memory impl (core/tool) does not
// accept the middlewares key.
type AssemblySettings struct {
	Middlewares *Settings    `json:"middlewares,omitempty"`
	Dynamic     *tool.Policy `json:"dynamic,omitempty"`
}

// AssemblyFactory builds tool.Assembly/middleware: the same assembly
// as tool.Assembly/memory but with the settings-declared middleware
// chain attached.
type AssemblyFactory struct{}

// Spec implements resource.Factory.
func (AssemblyFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: tool.AssemblyKind,
		Impl: AssemblyImpl,
		Deps: []resource.Requirement{depToolSources.Spec()},
	}
}

// New implements resource.Factory.
func (AssemblyFactory) New(ctx context.Context, in resource.Input) (*tool.Assembly, error) {
	settings, err := resource.DecodeTyped[AssemblySettings](ctx, in.Settings)
	if err != nil {
		return nil, errdefs.Validationf(
			"tool middleware: decode assembly settings: %w", err)
	}

	sources, err := depToolSources.GetAll(in)
	if err != nil {
		return nil, err
	}

	// An absent middlewares block is not an empty chain: FromSettings
	// installs the entries that are defaults rather than opt-ins (Recover,
	// the non-text part budget), so a document that says nothing still gets
	// the guarantees the guides promise it.
	middlewares := Settings{}
	if settings.Middlewares != nil {
		middlewares = *settings.Middlewares
	}
	mws, err := FromSettings(middlewares)
	if err != nil {
		return nil, err
	}
	opts := []tool.AssemblyOption{tool.WithMiddleware(mws...)}
	if settings.Dynamic != nil {
		opts = append(opts, tool.WithDynamic(*settings.Dynamic))
	}
	return tool.NewAssembly(sources, opts...)
}

// Register adds the middleware-enabled assembly factory to r.
func Register(r *resource.Registry) error {
	return resource.Register(r, AssemblyFactory{})
}
