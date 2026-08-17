package app

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/backends/plugin"
	"github.com/GizClaw/flowcraft/core/agent/scriptrt"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/event"
	graphresource "github.com/GizClaw/flowcraft/core/graph/resource"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/middleware"
	"github.com/GizClaw/flowcraft/core/workspace"

	"github.com/GizClaw/flowcraft/driver/deepseek"
	"github.com/GizClaw/flowcraft/driver/openai"

	"github.com/GizClaw/flowcraft/examples/forge/internal/simtools"
)

func buildRuntimeFromDocument(
	ctx context.Context,
	a *App,
	workspaceDir string,
	rest []byte,
	pluginsCfg *plugin.PluginsConfig,
) (*runtime.Runtime, error) {
	loader := resource.NewLoader(resource.WithBaseDir(a.dir))
	reg := resource.NewRegistry()

	if err := event.Register(reg); err != nil {
		return nil, err
	}
	if err := graphresource.Register(reg); err != nil {
		return nil, err
	}
	if err := workspace.Register(reg); err != nil {
		return nil, err
	}
	if err := tool.Register(reg); err != nil {
		return nil, err
	}
	if err := middleware.Register(reg); err != nil {
		return nil, err
	}
	if err := inference.Register(reg); err != nil {
		return nil, err
	}
	if err := scriptrt.Register(reg); err != nil {
		return nil, err
	}
	if err := openai.Register(reg); err != nil {
		return nil, err
	}
	if err := deepseek.Register(reg); err != nil {
		return nil, err
	}
	if err := reg.Register(simtools.NewSourceFactory(&a.toolCalls)); err != nil {
		return nil, fmt.Errorf("register sim tools: %w", err)
	}

	doc, err := deploy.Parse(rest)
	if err != nil {
		return nil, err
	}
	if pluginsCfg != nil {
		set, err := loadPluginSet(ctx, workspaceDir, *pluginsCfg)
		if err != nil {
			return nil, err
		}
		if err := set.Apply(ctx, &plugin.Target{Resources: reg}); err != nil {
			return nil, fmt.Errorf("apply plugins: %w", err)
		}
		// Merge the declaration-layer slots over the stripped base
		// document with the same LoadLayers semantics any deploy
		// consumer uses; the plugin set stays alive until App.Close
		// so service-slot processes outlive the runtime.
		base := deploy.Layer{
			Name:     "deploy.yaml",
			Priority: 0,
			Source:   resource.Source{Inline: rest},
		}
		doc, _, err = deploy.LoadLayers(
			ctx, append([]deploy.Layer{base}, set.Layers...))
		if err != nil {
			return nil, fmt.Errorf("merge plugin layers: %w", err)
		}
		a.plugins = set
	}

	builder := runtime.NewBuilder(reg)
	if err := builder.WithLoader(loader); err != nil {
		return nil, err
	}
	if err := builder.WithHostFactory(usageHostDecorator(a)); err != nil {
		return nil, err
	}
	rt, err := builder.Build(ctx, doc)
	if err != nil {
		return nil, fmt.Errorf("build runtime: %w", err)
	}
	return rt, nil
}
