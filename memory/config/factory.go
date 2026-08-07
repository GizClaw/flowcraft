package config

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/config/utils"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func decodeSettings(data []byte) (Settings, error) {
	return utils.Decode[Settings](data)
}

// Factory returns the flowcraft memory implementation factory. It is
// registered by applications into the generic sdk/memory assembly,
// mirroring how inference provider factories are registered.
//
// The flowcraft implementation consumes deployment dependencies:
//   - "inference" (*inference.Runtime, required);
//   - "workspace" (workspace.Workspace, required when storage selects the
//     "workspace" driver or when the lifecycle outbox is enabled).
func Factory() sdkmemory.Factory {
	return sdkmemory.FactoryFunc(func(ctx context.Context, input sdkmemory.Input) (sdkmemory.Assembly, error) {
		rawInference, ok := input.Deps["inference"]
		if !ok {
			return nil, errdefs.NotFoundf("memory config: dependency \"inference\" is required")
		}
		runtime, ok := rawInference.(*inference.Runtime)
		if !ok || runtime == nil {
			return nil, errdefs.Validationf("memory config: dependency \"inference\" has wrong type")
		}
		settings, err := decodeSettings(input.Settings)
		if err != nil {
			return nil, err
		}
		if settings.Storage.IsEmpty() {
			return nil, errdefs.Validationf(
				"memory config: storage.log and storage.kv drivers are required in memory.yaml")
		}
		registry := NewDriverRegistry()
		var ws workspace.Workspace
		if rawWorkspace, ok := input.Deps["workspace"]; ok {
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
	})
}
