package bootstrap

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/pluginhost"
	"github.com/GizClaw/flowcraft/internal/sandbox"
	"github.com/GizClaw/flowcraft/plugin"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workspace"

	otellog "go.opentelemetry.io/otel/log"
)

// wirePlugin creates the plugin registry and external manager, loads plugin
// configs, builds the node factory with a plugin-aware fallback, and injects
// plugin-provided tools. The returned cleanup shuts down all plugins.
func wirePlugin(
	ctx context.Context,
	cfg *config.Config,
	ws workspace.Workspace,
	workspaceRoot string,
	llmResolver llm.LLMResolver,
	toolReg *tool.Registry,
	schemaReg *node.SchemaRegistry,
	sandboxMgr *sandbox.Manager,
) (*pluginhost.Registry, *node.Factory, func(), error) {
	pluginReg := pluginhost.NewRegistry()

	pluginDir := cfg.Plugin.Dir
	if pluginDir == "" {
		pluginDir = filepath.Join(workspaceRoot, "plugins")
	}
	healthInterval := time.Duration(cfg.Plugin.HealthInterval) * time.Second
	if healthInterval <= 0 {
		healthInterval = 10 * time.Second
	}
	maxFailures := cfg.Plugin.MaxFailures
	if maxFailures <= 0 {
		maxFailures = 3
	}
	extMgr := pluginhost.NewExternalManager(pluginhost.ExternalManagerConfig{
		PluginDir:      pluginDir,
		HealthInterval: healthInterval,
		MaxFailures:    maxFailures,
	})
	pluginReg.SetExternalManager(extMgr)

	if cfg.Plugin.ConfigFile != "" {
		if configs, err := pluginhost.LoadPluginsJSON(cfg.Plugin.ConfigFile); err != nil {
			telemetry.Warn(ctx, "plugin: load config failed", otellog.String("error", err.Error()))
		} else {
			for _, pc := range configs {
				if pc.Path != "" && pc.Enabled {
					ep := pluginhost.NewExternalPlugin(pc.Path, plugin.PluginInfo{
						ID:   pc.ID,
						Name: pc.ID,
					})
					_ = pluginReg.RegisterWithConfig(ep, pc.Config)
				}
			}
		}
	}

	_ = pluginReg.InitializeAll(ctx)
	pluginhost.InjectSchemas(pluginReg, schemaReg)

	scriptRT := jsrt.New()
	nodeFactory := node.NewFactory(
		node.WithLLMResolver(llmResolver),
		node.WithToolRegistry(toolReg),
		node.WithScriptRuntime(scriptRT),
		node.WithWorkspace(ws),
		node.WithCommandRunner(workspace.NewLocalCommandRunner(workspaceRoot)),
	)

	jsFallback := nodeFactory.Fallback()
	nodeFactory.SetFallback(func(def graph.NodeDefinition, bctx *node.BuildContext) (graph.Node, error) {
		if p, ok := pluginReg.GetPluginForNodeType(def.Type); ok {
			if ep, ok := p.(*pluginhost.ExternalPlugin); ok {
				resolver := func(pluginID string) (plugin.NodeServiceClient, error) {
					rp, rok := pluginReg.Get(pluginID)
					if !rok {
						return nil, fmt.Errorf("plugin %q not registered", pluginID)
					}
					rep, rok := rp.(*pluginhost.ExternalPlugin)
					if !rok {
						return nil, fmt.Errorf("plugin %q is not external", pluginID)
					}
					client := rep.NodeClient()
					if client == nil {
						return nil, fmt.Errorf("plugin %q has no node service", pluginID)
					}
					return client, nil
				}
				host := &pluginhost.HostCallbackProvider{
					LLMGenerate: func(ctx context.Context, prompt string) (string, error) {
						m, err := llmResolver.Resolve(ctx, "")
						if err != nil {
							return "", fmt.Errorf("plugin llm callback: %w", err)
						}
						resp, _, err := m.Generate(ctx, []llm.Message{llm.NewTextMessage(llm.RoleUser, prompt)})
						if err != nil {
							return "", err
						}
						return resp.Content(), nil
					},
					ToolExecute: func(ctx context.Context, name, args string) (string, error) {
						t, ok := toolReg.Get(name)
						if !ok {
							return "", fmt.Errorf("tool %q not found", name)
						}
						return t.Execute(ctx, args)
					},
					SandboxExec: func(ctx context.Context, command string) (string, error) {
						if handle, ok := model.SandboxHandleFrom(ctx).(*sandbox.SandboxHandle); ok && handle != nil {
							sb, done, err := handle.Acquire(ctx)
							if err != nil {
								return "", fmt.Errorf("plugin sandbox callback: %w", err)
							}
							defer done()
							result, err := sb.Exec(ctx, "sh", []string{"-c", command}, sandbox.ExecOptions{})
							if err != nil {
								return "", err
							}
							return result.Stdout, nil
						}
						if sandboxMgr == nil {
							return "", fmt.Errorf("plugin sandbox callback: sandbox manager not available")
						}
						runtimeID := model.RuntimeIDFrom(ctx)
						if runtimeID == "" {
							return "", fmt.Errorf("plugin sandbox callback: no runtime ID in context")
						}
						sb, err := sandboxMgr.Acquire(ctx, runtimeID, sandbox.AcquireOptions{
							Mode: sandbox.ModePersistent,
						})
						if err != nil {
							return "", fmt.Errorf("plugin sandbox callback: %w", err)
						}
						defer func() { _ = sandboxMgr.Release(runtimeID) }()
						result, err := sb.Exec(ctx, "sh", []string{"-c", command}, sandbox.ExecOptions{})
						if err != nil {
							return "", err
						}
						return result.Stdout, nil
					},
					Signal: func(_ context.Context, _ string, _ any) error {
						return nil
					},
				}
				return pluginhost.NewProxyNode(def.ID, def.Type, ep.Info().ID, def.Config, resolver, host), nil
			}
		}
		if jsFallback != nil {
			return jsFallback(def, bctx)
		}
		return nil, fmt.Errorf("unknown node type %q for node %q", def.Type, def.ID)
	})

	pluginhost.InjectTools(pluginReg, toolReg)

	cleanup := func() {
		pluginReg.ShutdownAll(context.Background())
		extMgr.Stop(context.Background())
	}
	return pluginReg, nodeFactory, cleanup, nil
}
