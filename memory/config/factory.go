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
// The flowcraft implementation consumes two deployment dependencies:
//   - "inference" (*inference.Runtime, required);
//   - "workspace" (workspace.Workspace, required until the storage
//     backend refactor replaces workspace-backed stores with Log/KV).
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
		rawWorkspace, ok := input.Deps["workspace"]
		if !ok {
			return nil, errdefs.NotFoundf("memory config: dependency \"workspace\" is required")
		}
		ws, ok := rawWorkspace.(workspace.Workspace)
		if !ok || nilInterface(ws) {
			return nil, errdefs.Validationf("memory config: dependency \"workspace\" has wrong type")
		}
		settings, err := decodeSettings(input.Settings)
		if err != nil {
			return nil, err
		}
		builder, err := NewBuilder(ws, runtime)
		if err != nil {
			return nil, err
		}
		built, err := builder.NewAssembly(ctx, settings)
		if err != nil {
			return nil, err
		}
		return built, nil
	})
}
