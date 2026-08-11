package runtime

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdktool "github.com/GizClaw/flowcraft/sdk/tool"
	toolconfig "github.com/GizClaw/flowcraft/sdk/tool/config"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
	"github.com/GizClaw/flowcraft/sdkx/tool/dynamic"
)

// dynamicCatalogDefaultAgent is the reserved tools key used as the
// fallback assembly for agents without an explicit entry.
const dynamicCatalogDefaultAgent = "default"

// resolveDynamicCatalogAssemblies validates the agent -> tool resource
// mapping against the deployment and resolves every referenced
// assembly. Unknown agent keys are typos and fail the build; every
// deployed agent must have an explicit entry unless a default is
// declared.
func resolveDynamicCatalogAssemblies(
	doc deploy.Document,
	result *deploy.Result,
	cfg *DynamicCatalogConfig,
) (map[string]*toolconfig.Assembly, error) {
	for agentID := range cfg.Tools {
		if agentID == dynamicCatalogDefaultAgent {
			continue
		}
		if _, deployed := doc.Agents[agentID]; !deployed {
			return nil, errdefs.Validationf(
				"runtime config sessions.dynamic_catalog.tools[%q]: no such deployed agent",
				agentID)
		}
	}
	_, hasDefault := cfg.Tools[dynamicCatalogDefaultAgent]
	for agentID := range doc.Agents {
		if _, mapped := cfg.Tools[agentID]; !mapped && !hasDefault {
			return nil, errdefs.Validationf(
				"runtime config sessions.dynamic_catalog.tools: agent %q has no tool resource and no default",
				agentID)
		}
	}

	assemblies := make(map[string]*toolconfig.Assembly, len(cfg.Tools))
	resolved := make(map[string]*toolconfig.Assembly, len(cfg.Tools))
	for agentID, resourceName := range cfg.Tools {
		assembly, exists := resolved[resourceName]
		if !exists {
			var err error
			assembly, err = deploy.ResourceAs[*toolconfig.Assembly](result, resourceName)
			if err != nil {
				return nil, fmt.Errorf(
					"runtime config sessions.dynamic_catalog.tools[%q]: resolve %q: %w",
					agentID, resourceName, err)
			}
			resolved[resourceName] = assembly
		}
		assemblies[agentID] = assembly
	}
	return assemblies, nil
}

// newDynamicCatalogProvider builds one dynamic catalog per session over
// the mapped assembly's shared registry. The catalog is per-session
// state: each Session gets its own injection view, while the registry
// and the exposure metadata stay shared. The Session owns the catalog
// and closes it exactly once at session close.
func newDynamicCatalogProvider(
	assemblies map[string]*toolconfig.Assembly,
	cfg *DynamicCatalogConfig,
) session.CatalogProvider {
	return session.CatalogProviderFunc(func(
		ctx context.Context,
		instance *deploy.Instance,
	) (sdktool.Catalog, error) {
		assembly := assemblies[instance.Agent.ID]
		if assembly == nil {
			assembly = assemblies[dynamicCatalogDefaultAgent]
		}
		if assembly == nil || assembly.Registry() == nil {
			return nil, errdefs.Internalf(
				"runtime dynamic catalog: agent %q has no tool assembly",
				instance.Agent.ID)
		}
		reg := assembly.Registry()

		// Build a fresh policy per session: the catalog mutates its
		// policy map (SetExposure, Register), so sharing one map across
		// sessions would leak state and race.
		policy := dynamic.Policy{
			Default:           cfg.DefaultExposure,
			Exposures:         make(map[string]dynamic.Exposure, len(cfg.Exposures)),
			SelectedRetention: cfg.SelectedRetention,
			RecentWindow:      cfg.RecentWindow,
			Budget: dynamic.Budget{
				MaxDefinitions: cfg.Budget.MaxDefinitions,
				MaxBytes:       cfg.Budget.MaxBytes,
			},
		}
		catalog := dynamic.New(reg, dynamic.WithPolicy(policy))

		if err := dynamic.RegisterSearchTool(reg, catalog); err != nil {
			_ = catalog.Close()
			return nil, fmt.Errorf(
				"runtime dynamic catalog: register tool_search: %w", err)
		}
		for _, source := range assembly.Sources() {
			if exposures, ok := source.(dynamic.ExposureSource); ok {
				if err := exposures.ApplyExposures(catalog); err != nil {
					_ = catalog.Close()
					return nil, fmt.Errorf(
						"runtime dynamic catalog: apply exposures: %w", err)
				}
			}
		}
		// Config-declared exposures are host policy and win over the
		// tool source's own metadata.
		for name, exposure := range cfg.Exposures {
			if err := catalog.SetExposure(name, exposure); err != nil {
				_ = catalog.Close()
				return nil, fmt.Errorf(
					"runtime dynamic catalog: set exposure %q: %w", name, err)
			}
		}
		return catalog, nil
	})
}
