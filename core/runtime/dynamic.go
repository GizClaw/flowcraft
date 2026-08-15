package runtime

import (
	"context"
	"sync"

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

// catalogRegistry is the live agentID → tool.Assembly mapping behind the
// dynamic catalog. It is safe for concurrent use: the build-time mapping
// is seeded once, then RegisterAgent/UnregisterAgent update it at runtime.
// It implements session.CatalogProvider so the session manager consumes
// it directly.
type catalogRegistry struct {
	mu         sync.RWMutex
	assemblies map[string]*tool.Assembly
	def        *tool.Assembly
}

func newCatalogRegistry(assemblies map[string]*tool.Assembly) *catalogRegistry {
	c := &catalogRegistry{
		assemblies: make(map[string]*tool.Assembly, len(assemblies)),
	}
	for id, assembly := range assemblies {
		if id == dynamicCatalogDefaultAgent {
			c.def = assembly
			continue
		}
		c.assemblies[id] = assembly
	}
	return c
}

// NewCatalog implements session.CatalogProvider: one tool session per
// Session over the mapped assembly, falling back to the default.
func (c *catalogRegistry) NewCatalog(
	_ context.Context,
	instance *agent.Agent,
) (tool.Session, error) {
	if c == nil {
		return nil, nil
	}
	if instance == nil {
		return nil, errdefs.Internalf(
			"runtime dynamic catalog: nil agent instance")
	}
	c.mu.RLock()
	assembly := c.assemblies[instance.ID]
	def := c.def
	c.mu.RUnlock()
	if assembly == nil {
		assembly = def
	}
	if assembly == nil {
		return nil, errdefs.Internalf(
			"runtime dynamic catalog: agent %q has no tool assembly",
			instance.ID)
	}
	return assembly.NewSession(), nil
}

// Set updates the mapping for one agent (or the default when id is the
// reserved default key).
func (c *catalogRegistry) Set(id string, assembly *tool.Assembly) {
	if c == nil || id == "" {
		return
	}
	c.mu.Lock()
	if id == dynamicCatalogDefaultAgent {
		c.def = assembly
	} else {
		c.assemblies[id] = assembly
	}
	c.mu.Unlock()
}

// Delete removes the mapping for one agent. The default entry is never
// removable.
func (c *catalogRegistry) Delete(id string) {
	if c == nil || id == "" || id == dynamicCatalogDefaultAgent {
		return
	}
	c.mu.Lock()
	delete(c.assemblies, id)
	c.mu.Unlock()
}

// hasDefault reports whether a fallback tool assembly is configured.
func (c *catalogRegistry) hasDefault() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.def != nil
}

var _ session.CatalogProvider = (*catalogRegistry)(nil)
