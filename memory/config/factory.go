package config

import (
	"context"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/config/utils"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func decodeSettings(data []byte) (Settings, error) {
	return utils.Decode[Settings](data)
}

// ResourceKind is the deployment resource kind implemented by the
// flowcraft memory assembly.
const ResourceKind = "memory.Assembly"

// Factory returns the flowcraft memory deployment factory, an
// implementation of config.Factory (kind memory.Assembly, impl
// flowcraft). Applications register it on a deploy Builder:
//
//	builder.MustRegisterResource(flowcraftmemory.Factory())
//
// The flowcraft implementation consumes deployment dependencies:
//   - "inference" (*inference.Runtime, required);
//   - "workspace" (workspace.Workspace, required when storage selects the
//     "workspace" driver or when the lifecycle outbox is enabled).
func Factory() sdkconfig.Factory {
	return flowcraftFactory{}
}

type flowcraftFactory struct{}

// Spec implements config.Factory.
func (flowcraftFactory) Spec() sdkconfig.Spec {
	return sdkconfig.Spec{
		Kind:     ResourceKind,
		Impl:     "flowcraft",
		ItemType: "memory.System",
		Deps: []sdkconfig.DepSpec{
			{Name: "inference", Type: "inference.Runtime", Required: true},
			{Name: "workspace", Type: "workspace.Workspace", Required: true},
		},
	}
}

// New implements config.Factory: the settings subtree is the memory
// document, resolved through the input's shared loader and built into
// an Assembly over the bound inference runtime and workspace.
func (flowcraftFactory) New(ctx context.Context, in sdkconfig.Input) (any, error) {
	rawInference, ok := in.Dep("inference")
	if !ok {
		return nil, errdefs.NotFoundf("memory config: dependency \"inference\" is required")
	}
	runtime, ok := rawInference.(*inference.Runtime)
	if !ok || runtime == nil {
		return nil, errdefs.Validationf("memory config: dependency \"inference\" has wrong type")
	}
	data, err := in.ResolveDocument(ctx)
	if err != nil {
		return nil, err
	}
	settings, err := decodeSettings(data)
	if err != nil {
		return nil, err
	}
	if settings.Storage.IsEmpty() {
		return nil, errdefs.Validationf(
			"memory config: storage.log and storage.kv drivers are required in memory.yaml")
	}
	registry := NewDriverRegistry()
	var ws workspace.Workspace
	if rawWorkspace, ok := in.Dep("workspace"); ok {
		ws, ok = rawWorkspace.(workspace.Workspace)
		if !ok || nilInterface(ws) {
			return nil, errdefs.Validationf("memory config: dependency \"workspace\" has wrong type")
		}
	}
	needsWorkspace := settings.Storage.Log.Driver == "workspace" ||
		settings.Storage.KV.Driver == "workspace"
	if needsWorkspace {
		if ws == nil {
			return nil, errdefs.NotFoundf(
				"memory config: dependency \"workspace\" is required for the workspace driver")
		}
		if err := RegisterWorkspaceBackends(registry, ws); err != nil {
			return nil, err
		}
	}
	backends, err := registry.Resolve(settings.Storage)
	if err != nil {
		return nil, err
	}
	settings.Storage.Log = BackendSettings{}
	settings.Storage.KV = BackendSettings{}
	builder, err := NewBuilder(backends, runtime)
	if err != nil {
		return nil, err
	}
	if ws != nil {
		builder.WithOutboxWorkspace(ws)
	}
	built, err := builder.NewAssembly(ctx, settings)
	if err != nil {
		return nil, err
	}
	return built, nil
}
