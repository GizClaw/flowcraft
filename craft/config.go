package craft

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

const runtimeExternalName = "craft.runtime"

// composeDefinitionLayers materializes the definition-level layers: either the
// inline deploy document or the declared base_layers.
func (c *Craft) composeDefinitionLayers() ([]deploy.Layer, error) {
	if c.def.Deploy != nil {
		wire := map[string]any{"version": c.def.Deploy.Version}
		if len(c.def.Deploy.Resources) > 0 {
			wire["resources"] = c.def.Deploy.Resources
		}
		if len(c.def.Deploy.Agents) > 0 {
			wire["agents"] = c.def.Deploy.Agents
		}
		if c.def.Deploy.Runtime != nil {
			wire["runtime"] = json.RawMessage(c.def.Deploy.Runtime.Bytes())
		}
		raw, err := json.Marshal(wire)
		if err != nil {
			return nil, errdefs.Internal(
				fmt.Errorf("craft: encode inline deploy document: %w", err))
		}
		return []deploy.Layer{{
			Name:    "craft-deploy",
			Source:  resource.Source{Inline: raw},
			BaseDir: c.opts.DefinitionDir,
			Embed:   c.opts.Assets,
		}}, nil
	}
	layers := make([]deploy.Layer, 0, len(c.def.BaseLayers))
	for i, layer := range c.def.BaseLayers {
		name := layer.Name
		if name == "" {
			name = fmt.Sprintf("base-%d", i)
		}
		source := resource.Source{}
		if layer.File != "" {
			source.File = layer.File
		} else {
			source.Embed = layer.Embed
		}
		layers = append(layers, deploy.Layer{
			Name:     name,
			Priority: layer.Priority,
			Source:   source,
			BaseDir:  c.opts.DefinitionDir,
			Embed:    c.opts.Assets,
		})
	}
	return layers, nil
}

// compose merges every layer for one runtime, resolves craft values and
// injects the craft.runtime external.
func (c *Craft) compose(
	ctx context.Context,
	key RuntimeKey,
	opts RuntimeOptions,
) (deploy.Document, []deploy.ExternalResource, *resource.ReferenceResolver, error) {
	layers := make([]deploy.Layer, 0,
		len(c.definitionLayers)+len(c.opts.Layers)+len(opts.Layers))
	layers = append(layers, c.definitionLayers...)
	for _, capability := range c.caps {
		if contributor, ok := capability.(Layers); ok {
			layers = append(layers, contributor.Layers()...)
		}
	}
	layers = append(layers, c.opts.Layers...)
	layers = append(layers, opts.Layers...)

	doc, _, err := deploy.LoadLayers(ctx, layers)
	if err != nil {
		return deploy.Document{}, nil, nil,
			fmt.Errorf("craft: compose layers: %w", err)
	}

	values := make(map[string]string, len(c.opts.Values)+len(opts.Values))
	for name, value := range c.opts.Values {
		values[name] = value
	}
	for name, value := range opts.Values {
		values[name] = value
	}
	resolver := c.resolverFor(values)

	externals := make([]deploy.ExternalResource, 0,
		len(c.opts.Externals)+len(opts.Externals)+1)
	externals = append(externals, c.opts.Externals...)
	externals = append(externals, opts.Externals...)
	externals = append(externals, deploy.ExternalResource{
		External: deploy.External{
			Name:     runtimeExternalName,
			Contract: RuntimeContextContract,
		},
		Value: RuntimeContext{
			Key:       key,
			Values:    values,
			ConfigDir: c.opts.ConfigDir,
			DataDir:   c.opts.DataDir,
			AppHome:   c.opts.AppHome,
		},
	})
	if pluginExternals := c.pluginExternals(); len(pluginExternals) > 0 {
		externals = append(externals, pluginExternals...)
		if err := c.mountPluginTools(&doc); err != nil {
			return deploy.Document{}, nil, nil, err
		}
		if err := c.mountPluginNodes(&doc); err != nil {
			return deploy.Document{}, nil, nil, err
		}
	}
	if err := injectExternalDeps(&doc, externals); err != nil {
		return deploy.Document{}, nil, nil, err
	}
	return doc, externals, resolver, nil
}

// resolverFor builds the ${craft:...} scheme over the built-in roots and
// the merged values; capability schemes override it on name collisions.
func (c *Craft) resolverFor(values map[string]string) *resource.ReferenceResolver {
	builtins := map[string]string{
		"APP_HOME":   c.opts.AppHome,
		"CONFIG_DIR": c.opts.ConfigDir,
		"DATA_DIR":   c.opts.DataDir,
		"VERSION":    Version,
	}
	base := resource.NewResolver(resource.SchemeFunc{
		SchemeName: "craft",
		Fn: func(_ context.Context, ref resource.Reference) (any, error) {
			name := strings.TrimSpace(ref.Path)
			if name == "" {
				return nil, errdefs.Validationf(
					"craft: ${craft:} requires a name")
			}
			if value, ok := values[name]; ok {
				return value, nil
			}
			if value, ok := builtins[name]; ok {
				return value, nil
			}
			return nil, errdefs.Validationf(
				"craft: reference ${craft:%s} is not defined", name)
		},
	})
	for _, capability := range c.caps {
		if contributor, ok := capability.(Schemes); ok {
			if resolver := contributor.Resolver(); resolver != nil {
				base = base.Merge(resolver)
			}
		}
	}
	return base
}

// injectExternalDeps declares every injected external in the document's
// runtime.external_deps, keeping existing declarations.
func injectExternalDeps(
	doc *deploy.Document,
	externals []deploy.ExternalResource,
) error {
	if doc == nil || len(externals) == 0 {
		return nil
	}
	raw := []byte("{}")
	if doc.Runtime != nil {
		raw = doc.Runtime.Bytes()
	}
	section := map[string]any{}
	if err := json.Unmarshal(raw, &section); err != nil {
		return errdefs.Validationf("craft: decode runtime section: %v", err)
	}
	existing := []map[string]any{}
	if value, ok := section["external_deps"]; ok {
		encoded, err := json.Marshal(value)
		if err != nil {
			return errdefs.Internal(fmt.Errorf(
				"craft: encode declared external deps: %w", err))
		}
		if err := json.Unmarshal(encoded, &existing); err != nil {
			return errdefs.Validationf(
				"craft: runtime.external_deps must be a list: %v", err)
		}
	}
	seen := make(map[string]struct{}, len(existing)+len(externals))
	for _, entry := range existing {
		if name, ok := entry["name"].(string); ok {
			seen[name] = struct{}{}
		}
	}
	for _, external := range externals {
		if _, ok := seen[external.Name]; ok {
			continue
		}
		existing = append(existing, map[string]any{
			"name":     external.Name,
			"contract": external.Contract,
		})
		seen[external.Name] = struct{}{}
	}
	section["external_deps"] = existing
	encoded, err := json.Marshal(section)
	if err != nil {
		return errdefs.Internal(fmt.Errorf(
			"craft: encode runtime section: %w", err))
	}
	opaque := resource.Opaque(encoded)
	doc.Runtime = &opaque
	return nil
}
