// Package config wires memory implementation factories into
// sdkx/deploy resources. It knows only the sdk/memory contracts; every
// concrete implementation (such as the flowcraft memory module) lives
// in its own module and is registered here by the application, exactly
// like inference provider factories.
package config

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

// ResourceKind is the deploy resource category for memory assemblies.
const ResourceKind = "memory.Assembly"

// NewDeployFactory returns a deploy factory for one memory
// implementation. The implementation is addressed in documents as the
// resource's impl, e.g.:
//
//	resources:
//	  memories:
//	    kind: memory.Assembly
//	    impl: flowcraft
//	    settings: {file: ./memory.yaml}
func NewDeployFactory(impl string, factory sdkmemory.Factory) deploy.ResourceFactory {
	return &deployFactory{impl: impl, factory: factory}
}

type deployFactory struct {
	impl    string
	factory sdkmemory.Factory
}

func (f *deployFactory) Spec() deploy.ResourceSpec {
	return deploy.ResourceSpec{
		Kind:     ResourceKind,
		Impl:     f.impl,
		ItemType: "memory.System",
		Deps: []deploy.ResourceDepSpec{
			{Name: "workspace", Type: "workspace.Workspace", Required: true},
			{Name: "inference", Type: "inference.Runtime", Required: true},
		},
	}
}

func (f *deployFactory) New(ctx context.Context, in deploy.ResourceInput) (any, error) {
	settings, err := deploy.DecodeSettings[deploy.SubDocument](in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"memory config: decode settings: %w", err))
	}
	data, err := settings.YAML()
	if err != nil {
		return nil, err
	}
	rawWorkspace, ok := in.Dep("workspace")
	if !ok {
		return nil, errdefs.NotFoundf(
			"memory resource: dependency %q is not bound", "workspace")
	}
	ws, ok := rawWorkspace.(workspace.Workspace)
	if !ok || ws == nil {
		return nil, errdefs.Validationf(
			"memory resource: dependency %q has Go type %T, want workspace.Workspace",
			"workspace", rawWorkspace)
	}
	rawInference, ok := in.Dep("inference")
	if !ok {
		return nil, errdefs.NotFoundf(
			"memory resource: dependency %q is not bound", "inference")
	}
	runtime, ok := rawInference.(*inference.Runtime)
	if !ok || runtime == nil {
		return nil, errdefs.Validationf(
			"memory resource: dependency %q has Go type %T, want *inference.Runtime",
			"inference", rawInference)
	}
	return f.factory.New(ctx, sdkmemory.Input{
		Workspace: ws,
		Inference: runtime,
		Settings:  data,
	})
}
