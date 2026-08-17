package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/GizClaw/flowcraft/backends/plugin"
	"github.com/GizClaw/flowcraft/backends/plugin/remote"
	"github.com/GizClaw/flowcraft/backends/plugin/service"
	"github.com/GizClaw/flowcraft/core/utils"
)

// forgeCoreVersion is the core version forge pins in go.mod; it
// satisfies plugin requires.core constraints. Keep it in sync with
// the go.mod require.
const forgeCoreVersion = "0.1.5"

// splitPluginsSection extracts the optional top-level plugins section
// from a deployment document before the strict deploy.Parse runs.
// deploy.Document is decoded with unknown fields rejected, so the
// plugins key must be removed first. The rest is returned as JSON
// (the no-plugins fast path returns the original bytes unchanged).
func splitPluginsSection(raw []byte) ([]byte, *plugin.PluginsConfig, error) {
	doc, err := utils.Decode[map[string]json.RawMessage](raw)
	if err != nil {
		return nil, nil, fmt.Errorf("decode deployment document: %w", err)
	}
	pluginsRaw, ok := doc["plugins"]
	if !ok || bytes.Equal(bytes.TrimSpace(pluginsRaw), []byte("null")) {
		return raw, nil, nil
	}
	config, err := plugin.ParsePluginsSection(pluginsRaw)
	if err != nil {
		return nil, nil, err
	}
	delete(doc, "plugins")
	rest, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, fmt.Errorf("re-encode deployment document: %w", err)
	}
	return rest, &config, nil
}

// loadPluginSet loads the workspace's plugin directory with the
// service-slot builder wired, resolving relative plugin dirs against
// the workspace. Service artifacts stay lazy: the process spawns on
// the first resource construction.
func loadPluginSet(
	ctx context.Context, workspaceDir string, config plugin.PluginsConfig,
) (*plugin.Set, error) {
	for i, dir := range config.Dirs {
		if !filepath.IsAbs(dir) {
			config.Dirs[i] = filepath.Join(workspaceDir, dir)
		}
	}
	loader := plugin.NewLoader(
		plugin.WithCoreVersion(forgeCoreVersion),
		plugin.WithServicePluginBuilder(func(
			manifest plugin.Manifest, spec service.Spec,
		) ([]plugin.Plugin, error) {
			p, err := remote.NewPlugin(manifest, spec)
			if err != nil {
				return nil, err
			}
			return []plugin.Plugin{p}, nil
		}),
	)
	set, err := loader.Load(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("load plugins: %w", err)
	}
	return set, nil
}
