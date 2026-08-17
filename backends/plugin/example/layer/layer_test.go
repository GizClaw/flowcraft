// Package layer_test verifies the shipped declaration-layer example
// against the plugin shell, keeping the example honest as the loader
// contract evolves.
package layer_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/backends/plugin"
)

func TestLayerExampleLoads(t *testing.T) {
	ctx := context.Background()
	set, err := plugin.NewLoader().Load(ctx, plugin.PluginsConfig{
		Dirs:    []string{"."},
		Enabled: []string{"acme.hello-layer"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Plugins) != 1 {
		t.Fatalf("plugins = %d, want 1", len(set.Plugins))
	}
	if set.Plugins[0].Manifest().Name != "acme.hello-layer" {
		t.Fatalf("plugin name = %q", set.Plugins[0].Manifest().Name)
	}
	if len(set.Layers) != 1 {
		t.Fatalf("layers = %d, want 1", len(set.Layers))
	}
	layer := set.Layers[0]
	if layer.Priority != 100 || layer.Source.File != "layers/10-hello.yaml" {
		t.Fatalf("layer = %+v", layer)
	}
}

func TestLayerExampleDisabledByDefault(t *testing.T) {
	// An absent whitelist enables nothing: the example must stay inert
	// unless explicitly enabled.
	set, err := plugin.NewLoader().Load(context.Background(), plugin.PluginsConfig{
		Dirs: []string{"."},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Plugins) != 0 {
		t.Fatalf("plugins = %d, want 0 without an enabled whitelist", len(set.Plugins))
	}
}
