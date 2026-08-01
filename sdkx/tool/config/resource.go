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

// NewDeployResource returns a sdkx/deploy resource constructor over
// registry and deps.
//
// This is a constructor rather than a plain ResourceFunc because the
// tool registry, the approver and the audit sink are Go values a
// document cannot name: YAML declares the POLICY over tools (scopes,
// middleware order, which sources to attach) while the host decides
// which tools exist and who approves gated calls.
//
// Source kinds stay opt-in on the returned Builder's behalf — register
// them on the Builder passed here, not on the deploy Builder:
//
//	tb := config.NewBuilder(registry, config.Deps{Approver: ask})
//	mcp.Register(tb)
//	b.RegisterResource(config.ResourceKind, "yaml", config.NewDeployResource(tb))
func NewDeployResource(builder *Builder) deploy.ResourceFunc {
	return func(ctx context.Context, in deploy.ResourceInput) (any, error) {
		if builder == nil {
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
		return builder.Build(ctx, doc)
	}
}
