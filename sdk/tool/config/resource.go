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

// DepSandbox is the optional resource-level dependency carrying the
// sandbox.Runner command tools are built over. It is declared so a
// deployment can bind one sandbox out of a sandbox.Registry:
//
//	resources:
//	  boxes:
//	    kind: sandbox.Registry
//	    impl: yaml
//	    settings: {file: ./sandboxes.yaml}
//	  tools:
//	    kind: tool.Assembly
//	    impl: yaml
//	    deps: {sandbox: boxes/main}
//	    settings: {file: ./tools.yaml}
//
// The value is passed to every source factory's Input.Deps; builtin
// tool factories (e.g. sdkx/tool/exec's sandbox-backed exec) consume
// it there. It is optional: an assembly that only wires MCP servers or
// host-constructed tools does not need it.
const DepSandbox = "sandbox"

// Spec implements config.Factory.
func (b *Builder) Spec() config.Spec {
	return config.Spec{
		Kind: ResourceKind,
		Impl: "yaml",
		Deps: []config.DepSpec{{
			Name:     DepSandbox,
			Type:     "sandbox.Runner",
			Required: false,
		}},
	}
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
	return b.build(ctx, doc, in.Deps)
}
