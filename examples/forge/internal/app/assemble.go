package app

import (
	"context"
	"fmt"
	"path/filepath"

	flowcraftmemory "github.com/GizClaw/flowcraft/memory/config"
	flowcraftruntime "github.com/GizClaw/flowcraft/memory/runtime"
	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	eventconfig "github.com/GizClaw/flowcraft/sdk/event/config"
	graphconfig "github.com/GizClaw/flowcraft/sdk/graph/config"
	inferenceconfig "github.com/GizClaw/flowcraft/sdk/inference/config"
	envresolver "github.com/GizClaw/flowcraft/sdk/inference/config/env"
	memoryconfig "github.com/GizClaw/flowcraft/sdk/memory/config"
	schedulerconfig "github.com/GizClaw/flowcraft/sdk/scheduler/config"
	toolconfig "github.com/GizClaw/flowcraft/sdk/tool/config"
	workspaceconfig "github.com/GizClaw/flowcraft/sdk/workspace/config"
	jsrt "github.com/GizClaw/flowcraft/sdkx/agent/jsrt"
	kanbanconfig "github.com/GizClaw/flowcraft/sdkx/delegation/kanban/config"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	"github.com/GizClaw/flowcraft/sdkx/inference/azure"
	"github.com/GizClaw/flowcraft/sdkx/inference/bytedance"
	"github.com/GizClaw/flowcraft/sdkx/inference/deepseek"
	"github.com/GizClaw/flowcraft/sdkx/inference/minimax"
	"github.com/GizClaw/flowcraft/sdkx/inference/openai"
	"github.com/GizClaw/flowcraft/sdkx/inference/qwen"
	memoryhook "github.com/GizClaw/flowcraft/sdkx/memory/hook"
	runtimecore "github.com/GizClaw/flowcraft/sdkx/runtime"
	delegationruntime "github.com/GizClaw/flowcraft/sdkx/runtime/integration/delegation"
	sdkscheduler "github.com/GizClaw/flowcraft/sdkx/scheduler"
	"gopkg.in/yaml.v3"

	"github.com/GizClaw/flowcraft/examples/forge/internal/simtools"
)

func buildRuntimeFromDocument(ctx context.Context, a *App, doc deploy.Document) (*runtimecore.Runtime, error) {
	engines := agent.NewRegistry()
	engines.MustRegister(graphconfig.NewFactory(graphconfig.WithBaseDir(a.dir)))
	simtools.Register(a.tools, &a.toolCalls)

	deployBuilder := deploy.NewBuilder(engines, deploy.WithBaseDir(a.dir))
	deployBuilder.MustRegisterResource(eventconfig.NewMemoryDeployFactory())

	schedulerBuilder := schedulerconfig.NewBuilder()
	if err := sdkscheduler.Register(schedulerBuilder); err != nil {
		return nil, err
	}
	deployBuilder.MustRegisterResource(schedulerconfig.NewDeployFactory("local", schedulerBuilder))
	deployBuilder.MustRegisterResource(kanbanconfig.NewMemoryDeployFactory())

	workspaceBuilder := workspaceconfig.NewBuilder(workspaceconfig.Deps{BaseDir: a.dir})
	deployBuilder.MustRegisterResource(workspaceconfig.NewDeployFactory(workspaceBuilder))
	deployBuilder.MustRegisterResource(jsrt.NewDeployFactory())

	toolBuilder := toolconfig.NewBuilder(toolconfig.Deps{})
	for _, name := range a.tools.Names() {
		if t, ok := a.tools.Get(name); ok {
			toolBuilder.RegisterBuiltin(t)
		}
	}
	deployBuilder.MustRegisterResource(toolconfig.NewDeployFactory(toolBuilder))

	factories := providerFactories()
	resolvers := map[string]inferenceconfig.SecretResolver{"env": envresolver.New()}
	deployBuilder.MustRegisterResource(inferenceconfig.NewDeployFactory(factories, resolvers))
	deployBuilder.MustRegisterResource(memoryconfig.NewDeployFactory(
		"flowcraft",
		flowcraftmemory.Factory(),
		sdkconfig.ResourceDepSpec{Name: "inference", Type: "inference.Runtime", Required: true},
		sdkconfig.ResourceDepSpec{Name: "workspace", Type: "workspace.Workspace", Required: true},
	))
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
	if err := runtimeBuilder.WithHostFactory(usageHostDecorator(a)); err != nil {
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

// absolutizeDeployment rewrites resource sub-document paths to
// absolute paths rooted at dir. The deployment schema itself is
// untouched; this is application-side file resolution, which the docs
// leave to the consumer. Workspace roots resolve against the host
// builder's BaseDir instead of a document field.
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
	}
	return yaml.Marshal(document)
}
