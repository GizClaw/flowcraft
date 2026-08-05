package app

import (
	"context"
	"fmt"
	"path/filepath"

	flowcraftmemory "github.com/GizClaw/flowcraft/memory/config"
	flowcraftruntime "github.com/GizClaw/flowcraft/memory/runtime"
	"github.com/GizClaw/flowcraft/sdk/agent"
	graphagent "github.com/GizClaw/flowcraft/sdkx/agent/graph"
	jsrt "github.com/GizClaw/flowcraft/sdkx/agent/jsrt"
	kanbanconfig "github.com/GizClaw/flowcraft/sdkx/delegation/kanban/config"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	eventconfig "github.com/GizClaw/flowcraft/sdkx/event/config"
	"github.com/GizClaw/flowcraft/sdkx/inference/azure"
	"github.com/GizClaw/flowcraft/sdkx/inference/bytedance"
	inferenceconfig "github.com/GizClaw/flowcraft/sdkx/inference/config"
	envresolver "github.com/GizClaw/flowcraft/sdkx/inference/config/env"
	inferenceyaml "github.com/GizClaw/flowcraft/sdkx/inference/config/yaml"
	"github.com/GizClaw/flowcraft/sdkx/inference/deepseek"
	"github.com/GizClaw/flowcraft/sdkx/inference/minimax"
	"github.com/GizClaw/flowcraft/sdkx/inference/openai"
	"github.com/GizClaw/flowcraft/sdkx/inference/qwen"
	sdkxmemory "github.com/GizClaw/flowcraft/sdkx/memory/config"
	memoryhook "github.com/GizClaw/flowcraft/sdkx/memory/hook"
	runtimecore "github.com/GizClaw/flowcraft/sdkx/runtime"
	delegationruntime "github.com/GizClaw/flowcraft/sdkx/runtime/integration/delegation"
	schedulerconfig "github.com/GizClaw/flowcraft/sdkx/scheduler/config"
	toolconfig "github.com/GizClaw/flowcraft/sdkx/tool/config"
	workspaceconfig "github.com/GizClaw/flowcraft/sdkx/workspace/config"
	"gopkg.in/yaml.v3"

	"github.com/GizClaw/flowcraft/examples/forge/internal/simtools"
)

func buildRuntimeFromDocument(ctx context.Context, a *App, doc deploy.Document) (*runtimecore.Runtime, error) {
	engines := agent.NewRegistry()
	engines.MustRegister(graphagent.NewFactory(graphagent.WithBaseDir(a.dir)))
	simtools.Register(a.tools, &a.toolCalls)

	deployBuilder := deploy.NewBuilder(engines, deploy.WithBaseDir(a.dir))
	deployBuilder.MustRegisterResource(eventconfig.NewMemoryDeployFactory())
	deployBuilder.MustRegisterResource(schedulerconfig.NewLocalDeployFactory())
	deployBuilder.MustRegisterResource(kanbanconfig.NewMemoryDeployFactory())
	deployBuilder.MustRegisterResource(workspaceconfig.NewDeployFactory())
	deployBuilder.MustRegisterResource(jsrt.NewDeployFactory())
	toolBuilder := toolconfig.NewBuilder(a.tools, toolconfig.Deps{})
	deployBuilder.MustRegisterResource(toolconfig.NewDeployFactory(toolBuilder))

	factories := providerFactories()
	resolvers := map[string]inferenceconfig.SecretResolver{"env": envresolver.New()}
	deployBuilder.MustRegisterResource(inferenceyaml.NewDeployFactory(factories, resolvers))
	deployBuilder.MustRegisterResource(sdkxmemory.NewDeployFactory("flowcraft", flowcraftmemory.Factory()))
	deployBuilder.RegisterPreparer(memoryhook.ContextType, memoryhook.ContextPreparerFactory)
	deployBuilder.RegisterCommitter(memoryhook.TurnType, memoryhook.TurnCommitterFactory)

	runtimeBuilder := runtimecore.NewBuilder(deployBuilder)
	delegationFactory, err := delegationruntime.NewFactory(a.tools)
	if err != nil {
		return nil, err
	}
	if err := runtimeBuilder.RegisterIntegration(delegationFactory); err != nil {
		return nil, err
	}
	if err := runtimeBuilder.RegisterIntegration(flowcraftruntime.NewFactory()); err != nil {
		return nil, err
	}
	if err := runtimeBuilder.RegisterIntegration(&debugIntegrationFactory{app: a}); err != nil {
		return nil, err
	}
	rt, err := runtimeBuilder.Build(ctx, doc)
	if err != nil {
		return nil, fmt.Errorf("build runtime: %w", err)
	}
	return rt, nil
}

func providerFactories() map[string]inferenceconfig.Factory {
	return map[string]inferenceconfig.Factory{
		"openai":    openai.Factory(),
		"azure":     azure.Factory(),
		"deepseek":  deepseek.Factory(),
		"qwen":      qwen.Factory(),
		"bytedance": bytedance.Factory(),
		"minimax":   minimax.Factory(),
	}
}

// absolutizeDeployment rewrites resource sub-document paths (and the
// workspace registry base dir) to absolute paths rooted at dir. The
// deployment schema itself is untouched; this is application-side file
// resolution, which the docs leave to the consumer.
func absolutizeDeployment(raw []byte, dir string) ([]byte, error) {
	var document map[string]any
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	resources, _ := document["resources"].(map[string]any)
	for _, entry := range resources {
		resource, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		settings, ok := resource["settings"].(map[string]any)
		if !ok {
			continue
		}
		if file, ok := settings["file"].(string); ok && file != "" && !filepath.IsAbs(file) {
			settings["file"] = filepath.Join(dir, file)
		}
		if kind, _ := resource["kind"].(string); kind == "workspace.Registry" {
			settings["base_dir"] = dir
		}
	}
	return yaml.Marshal(document)
}
