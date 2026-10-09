package craft

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/hooks"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/craft/plugin"
)

// PluginHost is the craft-facing view of a plugin host (implemented by
// plugin.Host).
type PluginHost interface {
	Start(ctx context.Context) error
	Close() error
	Revision() uint64
	Subscribe(fn func()) func()
	Tools() tool.Source
	SkillRoots() []string
	HookSources() []hooks.ExtraSource
	Entries() ([]plugin.Entry, error)
	CallTool(ctx context.Context, pluginID, toolName string, args any) (json.RawMessage, error)
}

const (
	// PluginHostContract is the dependency contract of the
	// craft.pluginhost external.
	PluginHostContract = "craft.PluginHost"

	pluginToolsExternalName = "craft.plugins"
	pluginHostExternalName  = "craft.pluginhost"
	pluginToolsDepKey       = "tool.plugins"
)

// Plugins returns the plugin host, or nil when this Craft has none.
func (c *Craft) Plugins() PluginHost {
	if c == nil {
		return nil
	}
	return c.plugins
}

// pluginExternals are the two Craft-level values injected into every
// runtime when a plugin host is configured.
func (c *Craft) pluginExternals() []deploy.ExternalResource {
	if c.plugins == nil {
		return nil
	}
	return []deploy.ExternalResource{{
		External: deploy.External{
			Name:     pluginToolsExternalName,
			Contract: "tool.Source",
		},
		Value: c.plugins.Tools(),
	}, {
		External: deploy.External{
			Name:     pluginHostExternalName,
			Contract: PluginHostContract,
		},
		Value: c.plugins,
	}}
}

// mountPluginTools adds the tool.plugins dependency to the configured
// mount point, or to every resource declaring a Many "tool" dep.
func (c *Craft) mountPluginTools(doc *deploy.Document) error {
	if c == nil || c.plugins == nil || doc == nil {
		return nil
	}
	target := ""
	if c.def.Plugins != nil {
		target = strings.TrimSpace(c.def.Plugins.ToolRegistry)
	}
	if target != "" {
		declared, ok := doc.Resources[target]
		if !ok {
			return errdefs.NotFoundf(
				"craft: plugins.tool_registry resource %q not found", target)
		}
		addToolSetDep(doc, target, declared)
		return nil
	}
	names := make([]string, 0, len(doc.Resources))
	for name := range doc.Resources {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		declared := doc.Resources[name]
		factory, ok := c.registry.Lookup(declared.Kind, declared.Impl)
		if !ok {
			continue
		}
		if !declaresManyDep(factory.Spec(), "tool") {
			continue
		}
		addToolSetDep(doc, name, declared)
	}
	return nil
}

// mountPluginNodes synthesizes one graph.NodeType/mcp resource per
// plugin-declared node and wires node_type deps into the target agents.
func (c *Craft) mountPluginNodes(doc *deploy.Document) error {
	if c == nil || c.plugins == nil || doc == nil || c.def.Plugins == nil {
		return nil
	}
	targets := c.def.Plugins.NodeTargets
	if len(targets) == 0 {
		return nil
	}
	for _, target := range targets {
		if _, ok := doc.Agents[target]; !ok {
			return errdefs.NotFoundf(
				"craft: plugins.node_targets agent %q not found", target)
		}
	}
	entries, err := c.plugins.Entries()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		// The nodes section is gated by nodes:provide alone. The
		// mcp:provide grant is checked where the plugin's server is
		// opened, so a node backed by a plugin that lacks it is still
		// synthesized and fails at call time with the missing grant
		// instead of silently disappearing here.
		if !entry.Manifest.HasPermission("nodes:provide") {
			continue
		}
		for _, node := range entry.Manifest.Nodes {
			name := "plugin." + entry.ID + ".node." + node.Type
			settings := map[string]any{
				"type":   entry.ID + "." + node.Type,
				"plugin": entry.ID,
				"tool":   node.Tool,
			}
			if node.Timeout != "" {
				settings["timeout"] = node.Timeout
			}
			raw, err := json.Marshal(settings)
			if err != nil {
				return errdefs.Internal(err)
			}
			doc.Resources[name] = resource.Resource{
				Kind:     pluginNodeTypeKind,
				Impl:     pluginNodeTypeImpl,
				Settings: raw,
			}
			for _, target := range targets {
				definition := doc.Agents[target]
				if definition.Engine.Deps == nil {
					definition.Engine.Deps = resource.Deps{}
				}
				definition.Engine.Deps["node_type."+entry.ID+"."+node.Type] =
					resource.Ref(name)
				doc.Agents[target] = definition
			}
		}
	}
	return nil
}

func addToolSetDep(
	doc *deploy.Document,
	name string,
	declared resource.Resource,
) {
	if declared.Deps == nil {
		declared.Deps = resource.Deps{}
	}
	declared.Deps[pluginToolsDepKey] = resource.Ref(pluginToolsExternalName)
	doc.Resources[name] = declared
}

func declaresManyDep(spec resource.Spec, name string) bool {
	for _, dep := range spec.Deps {
		if dep.Name == name && dep.Many {
			return true
		}
	}
	return false
}

// startPluginWatch starts the revision watcher that reloads every open
// runtime after a plugin change.
func (c *Craft) startPluginWatch() {
	ctx, cancel := context.WithCancel(context.Background())
	signal := make(chan struct{}, 1)
	unsubscribe := c.plugins.Subscribe(func() {
		select {
		case signal <- struct{}{}:
		default:
		}
	})
	c.mu.Lock()
	c.watchCancel = cancel
	c.unsubscribe = unsubscribe
	c.mu.Unlock()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-signal:
				_ = c.ReloadAll(ctx, ReasonPlugin)
			}
		}
	}()
}
