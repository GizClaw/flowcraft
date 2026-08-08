package config

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ResourceKind is the deployment resource category this package builds.
// The bound value is an *Assembly: tools are selected per call by name,
// so consumers take the whole assembly rather than one item out of it.
const ResourceKind = "tool.Assembly"

// Spec implements config.Factory.
func (b *Builder) Spec() config.Spec {
	return config.Spec{Kind: ResourceKind, Impl: "yaml"}
}

// New implements config.Factory: the settings subtree is the tool
// policy document, resolved through the input's shared loader and
// built over the host builder.
func (b *Builder) New(ctx context.Context, in config.Input) (any, error) {
	if b == nil {
		return nil, errdefs.Validationf(
			"tool config: builder is nil")
	}
	data, err := in.ResolveDocument(ctx)
	if err != nil {
		return nil, err
	}
	doc, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return b.Build(ctx, doc)
}
