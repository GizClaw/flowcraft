package pluginhost

import (
	"encoding/json"

	"github.com/GizClaw/flowcraft/plugin"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// InjectSchemas collects node schemas from all active plugins and registers
// them into the given SchemaRegistry. This bridges the plugin's raw
// map[string]any schema format into typed NodeSchema values.
func InjectSchemas(reg *Registry, schemaReg *node.SchemaRegistry) {
	raw := reg.CollectNodeSchemas()
	for _, m := range raw {
		ns := mapToNodeSchema(m)
		if ns.Type == "" {
			continue
		}
		ns.Category = "plugin"
		schemaReg.Register(ns)
	}

	for _, nd := range reg.CollectNodeDefs() {
		if nd.Schema == nil {
			continue
		}
		ns := mapToNodeSchema(nd.Schema)
		if ns.Type == "" {
			ns.Type = nd.Type
		}
		if ns.Label == "" {
			ns.Label = nd.Type
		}
		ns.Category = "plugin"
		if _, ok := schemaReg.Get(ns.Type); !ok {
			schemaReg.Register(ns)
		}
	}
}

// InjectTools registers external plugin tools into the given tool Registry.
// Tools whose names collide with already-registered (built-in) tools are
// skipped to prevent accidental overrides.
func InjectTools(reg *Registry, toolReg *tool.Registry) {
	reg.mu.RLock()
	defer reg.mu.RUnlock()

	for _, mp := range reg.plugins {
		if mp.status != plugin.StatusActive {
			continue
		}
		ep, ok := mp.p.(*ExternalPlugin)
		if !ok {
			continue
		}
		toolClient := ep.ToolClient()
		if toolClient == nil {
			continue
		}
		for _, spec := range ep.Tools() {
			if _, exists := toolReg.Get(spec.Name); exists {
				continue
			}
			proxy := NewProxyTool(spec, ep.Info().ID, toolClient.Execute)
			toolReg.Register(proxy)
		}
	}
}

// CleanupSchemas removes plugin-category schemas from the SchemaRegistry
// that are no longer provided by any active plugin.
func CleanupSchemas(reg *Registry, schemaReg *node.SchemaRegistry) {
	activeTypes := make(map[string]bool)
	for _, nd := range reg.CollectNodeDefs() {
		activeTypes[nd.Type] = true
	}
	for _, m := range reg.CollectNodeSchemas() {
		if t, ok := m["type"].(string); ok {
			activeTypes[t] = true
		}
	}
	for _, schema := range schemaReg.All() {
		if schema.Category == "plugin" && !activeTypes[schema.Type] {
			schemaReg.Unregister(schema.Type)
		}
	}
}

// CleanupTools removes ProxyTool entries from the tool Registry
// that are no longer provided by any active plugin.
func CleanupTools(reg *Registry, toolReg *tool.Registry) {
	activeTools := make(map[string]bool)
	for _, spec := range reg.CollectToolSpecs() {
		activeTools[spec.Name] = true
	}
	for _, name := range toolReg.Names() {
		if activeTools[name] {
			continue
		}
		if t, ok := toolReg.Get(name); ok {
			if _, isProxy := t.(*ProxyTool); isProxy {
				toolReg.Unregister(name)
			}
		}
	}
}

func mapToNodeSchema(m map[string]any) node.NodeSchema {
	data, err := json.Marshal(m)
	if err != nil {
		return node.NodeSchema{}
	}
	var ns node.NodeSchema
	_ = json.Unmarshal(data, &ns)
	return ns
}
