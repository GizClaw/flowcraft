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

// NewDeployFactory returns the deployment factory for tool assemblies
// over the host's builder.
//
// The tool registry, approver and audit sink are Go values a document
// cannot name: the document declares the POLICY over tools (scopes,
// middleware order, which sources to attach) while the host decides
// which tools exist and who approves gated calls.
//
// Source kinds stay opt-in on the returned Builder's behalf — register
// them on the Builder passed here, not on the deployment engine:
//
//	tb := config.NewBuilder(registry, config.Deps{Approver: ask})
//	mcp.Register(tb)
//	builder.RegisterResource(config.NewDeployFactory(tb))
func NewDeployFactory(builder *Builder) config.ResourceFactory {
	return config.NewDocumentFactory(
		config.ResourceSpec{Kind: ResourceKind, Impl: "yaml"},
		func(ctx context.Context, data []byte, deps map[string]any) (any, error) {
			if builder == nil {
				return nil, errdefs.Validationf(
					"tool config: deploy factory builder is nil")
			}
			doc, err := Parse(data)
			if err != nil {
				return nil, err
			}
			return builder.Build(ctx, doc)
		},
	)
}
