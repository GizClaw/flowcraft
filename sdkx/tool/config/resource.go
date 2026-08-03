package config

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

// ResourceKind is the deploy resource category this package builds.
// The bound value is an *Assembly: tools are selected per call by
// name, so consumers take the whole assembly rather than one item out
// of it.
const ResourceKind = "tool.Assembly"

// ResourceSettings is the settings subtree of a tool resource.
type ResourceSettings struct {
	deploy.SubDocument `yaml:",inline"`
}

type deployFactory struct {
	builder *Builder
}

// NewDeployFactory returns the YAML deploy factory over a tool builder.
//
// The tool registry, approver and audit sink are Go values a
// document cannot name: YAML declares the POLICY over tools (scopes,
// middleware order, which sources to attach) while the host decides
// which tools exist and who approves gated calls.
//
// Source kinds stay opt-in on the returned Builder's behalf — register
// them on the Builder passed here, not on the deploy Builder:
//
//	tb := config.NewBuilder(registry, config.Deps{Approver: ask})
//	mcp.Register(tb)
//	b.RegisterResource(config.NewDeployFactory(tb))
func NewDeployFactory(builder *Builder) deploy.ResourceFactory {
	return &deployFactory{builder: builder}
}

func (*deployFactory) Spec() deploy.ResourceSpec {
	return deploy.ResourceSpec{Kind: ResourceKind, Impl: "yaml"}
}

func (f *deployFactory) New(ctx context.Context, in deploy.ResourceInput) (any, error) {
	if f.builder == nil {
		return nil, errdefs.Validationf("tool config: builder is nil")
	}
	settings, err := deploy.DecodeSettings[ResourceSettings](in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"tool config: decode resource settings: %w", err))
	}
	data, err := settings.YAML()
	if err != nil {
		return nil, err
	}
	doc, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return f.builder.Build(ctx, doc)
}
