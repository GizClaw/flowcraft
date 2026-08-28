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
		Deps: []resource.DepSpec{{
			Name: "tool", Type: "tool.Source", Required: true, Many: true,
		}},
	}
}

// New implements resource.Factory.
func (AssemblyFactory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[AssemblySettings](ctx, in.Settings)
	if err != nil {
		return nil, errdefs.Validationf(
			"tool middleware: decode assembly settings: %v", err)
	}

	values := in.DepsMany("tool")
	sources := make([]tool.Source, 0, len(values))
	for _, value := range values {
		src, ok := value.(tool.Source)
		if !ok {
			return nil, errdefs.Validationf(
				"tool middleware: assembly dep is %T, want tool.Source", value)
		}
		sources = append(sources, src)
	}
	if len(sources) == 0 {
		return nil, errdefs.Validationf(
			"tool middleware: assembly requires at least one tool source")
	}

	var opts []tool.AssemblyOption
	if settings.Middlewares != nil {
		mws, err := FromSettings(*settings.Middlewares)
		if err != nil {
			return nil, err
		}
		opts = append(opts, tool.WithMiddleware(mws...))
	}
	if settings.Dynamic != nil {
		opts = append(opts, tool.WithDynamic(*settings.Dynamic))
	}
	return tool.NewAssembly(sources, opts...)
}

// Register adds the middleware-enabled assembly factory to r.
func Register(r *resource.Registry) error {
	return r.Register(AssemblyFactory{})
}
