// Package yaml exposes the memory Assembly as a deploy resource.
package yaml

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	memoryconfig "github.com/GizClaw/flowcraft/sdkx/memory/config"
)

const ResourceKind = "memory.Assembly"

type deployFactory struct{}

func NewDeployFactory() deploy.ResourceFactory { return &deployFactory{} }

func (*deployFactory) Spec() deploy.ResourceSpec {
	return deploy.ResourceSpec{
		Kind: ResourceKind, Impl: "memory", ItemType: "memory.System",
		Deps: []deploy.ResourceDepSpec{
			{Name: "workspace", Type: "workspace.Workspace", Required: true},
			{Name: "inference", Type: "inference.Runtime", Required: true},
		},
	}
}

func (*deployFactory) New(ctx context.Context, input deploy.ResourceInput) (any, error) {
	settings, err := deploy.DecodeSettings[memoryconfig.Settings](input.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory resource: decode settings: %w", err))
	}
	rawWorkspace, ok := input.Dep("workspace")
	if !ok {
		return nil, errdefs.NotFoundf("memory resource: required dependency %q is not bound", "workspace")
	}
	ws, ok := rawWorkspace.(workspace.Workspace)
	if !ok || ws == nil {
		return nil, errdefs.Validationf(
			"memory resource: dependency %q has Go type %T, want workspace.Workspace",
			"workspace", rawWorkspace,
		)
	}
	rawInference, ok := input.Dep("inference")
	if !ok {
		return nil, errdefs.NotFoundf("memory resource: required dependency %q is not bound", "inference")
	}
	runtime, ok := rawInference.(*inference.Runtime)
	if !ok || runtime == nil {
		return nil, errdefs.Validationf(
			"memory resource: dependency %q has Go type %T, want *inference.Runtime",
			"inference", rawInference,
		)
	}
	builder, err := memoryconfig.NewBuilder(ws, runtime)
	if err != nil {
		return nil, errdefs.Validation(err)
	}
	assembly, err := builder.NewAssembly(ctx, settings)
	if err != nil {
		return nil, errdefs.Validation(err)
	}
	return assembly, nil
}
