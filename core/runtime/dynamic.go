package runtime

import (
	"context"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/runtime/session"
	"github.com/GizClaw/flowcraft/core/tool"
)

// dynamicCatalogDefaultAgent is the reserved tools key used as the
// fallback assembly for agents without an explicit entry.
const dynamicCatalogDefaultAgent = "default"

// resolveDynamicCatalogAssemblies validates the agent -> tool.Assembly
// mapping against the deployment and resolves every referenced
// assembly. Unknown agent keys are typos and fail the build; every
// deployed agent must have an explicit entry unless a default is
// declared.
func resolveDynamicCatalogAssemblies(
	doc deploy.Document,
	result *deploy.Result,
	cfg *DynamicCatalogConfig,
) (map[string]*tool.Assembly, error) {
	for agentID := range cfg.Tools {
		if agentID == dynamicCatalogDefaultAgent {
			continue
		}
		if _, deployed := doc.Agents[agentID]; !deployed {
			return nil, errdefs.Validationf(
				"runtime config dynamic_catalog.tools[%q]: no such deployed agent",
				agentID)
		}
	}
	_, hasDefault := cfg.Tools[dynamicCatalogDefaultAgent]
	for agentID := range doc.Agents {
		if _, mapped := cfg.Tools[agentID]; !mapped && !hasDefault {
			return nil, errdefs.Validationf(
				"runtime config dynamic_catalog.tools: agent %q has no tool resource and no default",
				agentID)
		}
	}

	assemblies := make(map[string]*tool.Assembly, len(cfg.Tools))
	resolved := make(map[string]*tool.Assembly, len(cfg.Tools))
	for agentID, resourceName := range cfg.Tools {
		assembly, exists := resolved[resourceName]
		if !exists {
			value, ok := result.Value(resourceName)
			if !ok {
				return nil, errdefs.NotFoundf(
					"runtime config dynamic_catalog.tools[%q]: resource %q not found",
					agentID, resourceName)
			}
			var typeOK bool
			assembly, typeOK = value.(*tool.Assembly)
			if !typeOK || assembly == nil {
				return nil, errdefs.Validationf(
					"runtime config dynamic_catalog.tools[%q]: resource %q is %T, want *tool.Assembly",
					agentID, resourceName, value)
			}
			resolved[resourceName] = assembly
		}
		assemblies[agentID] = assembly
	}
	return assemblies, nil
}

// newDynamicCatalogProvider builds one tool session per Session over
// the mapped assembly. The Session borrows the view and never closes
// it.
func newDynamicCatalogProvider(
	assemblies map[string]*tool.Assembly,
) session.CatalogProvider {
	return session.CatalogProviderFunc(func(
		ctx context.Context,
		instance *agent.Agent,
	) (tool.Session, error) {
		assembly := assemblies[instance.ID]
		if assembly == nil {
			assembly = assemblies[dynamicCatalogDefaultAgent]
		}
		if assembly == nil {
			return nil, errdefs.Internalf(
				"runtime dynamic catalog: agent %q has no tool assembly",
				instance.ID)
		}
		return assembly.NewSession(), nil
	})
}
