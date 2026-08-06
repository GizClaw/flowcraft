package config

import (
	"context"
	"fmt"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// ResourceKind is the deployment resource category for memory
// assemblies.
const ResourceKind = "memory.Assembly"

// ResourceSettings is the settings subtree of a memory resource: where
// its implementation-owned sub-document lives.
type ResourceSettings struct {
	sdkconfig.SubDocument
}

type deployFactory struct {
	impl    string
	factory sdkmemory.Factory
}

// NewDeployFactory returns a deployment factory for one memory
// implementation. The implementation is addressed in documents as the
// resource's impl, e.g.:
//
//	resources:
//	  memories:
//	    kind: memory.Assembly
//	    impl: flowcraft
//	    settings: {file: ./memory.yaml}
func NewDeployFactory(impl string, factory sdkmemory.Factory) sdkconfig.ResourceFactory {
	return &deployFactory{impl: impl, factory: factory}
}

func (f *deployFactory) Spec() sdkconfig.ResourceSpec {
	return sdkconfig.ResourceSpec{
		Kind:     ResourceKind,
		Impl:     f.impl,
		ItemType: "memory.System",
		Deps: []sdkconfig.ResourceDepSpec{
			{Name: "workspace", Type: "workspace.Workspace", Required: true},
			{Name: "inference", Type: "inference.Runtime", Required: true},
		},
	}
}

func (f *deployFactory) New(ctx context.Context, in sdkconfig.Input) (any, error) {
	if f.factory == nil {
		return nil, errdefs.Validationf(
			"memory config: deploy factory has no implementation factory")
	}
	var settings ResourceSettings
	if err := in.Settings.Decode(&settings); err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"memory config: decode resource settings: %w", err))
	}
	data, err := settings.Bytes()
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
