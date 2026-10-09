package craft

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/craft/hostmcp"
	"github.com/GizClaw/flowcraft/craft/plugin"
)

type wiringSecrets struct{}

func (wiringSecrets) Get(context.Context, string, string) (string, error) {
	return "value", nil
}
func (wiringSecrets) Set(context.Context, string, string, string) error { return nil }
func (wiringSecrets) Delete(context.Context, string, string) error      { return nil }

func TestCraftInjectsHostMCPEnv(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "hello")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{
		"id": "hello", "version": "0.1.0",
		"permissions": ["mcp:provide", "secrets:auth"],
		"mcp": {"command": "true"}
	}`), 0o600); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	store, err := plugin.NewStore(plugin.Options{
		Roots:       []plugin.Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	var captured map[string]string
	host, err := plugin.NewHost(plugin.HostOptions{
		Store: store,
		NewSource: func(
			_ context.Context,
			_ plugin.Entry,
			env map[string]string,
			_ string,
		) (plugin.Source, error) {
			captured = map[string]string{}
			for key, value := range env {
				captured[key] = value
			}
			return &mountSource{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	def, err := ParseDefinition([]byte(`
craft:
  id: test
  version: 0.1.0
deploy:
  version: v1
  resources:
    bus: {kind: event.Bus, impl: memory}
  runtime:
    event_bus: bus
`))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	c, err := New(def, Options{
		DataDir:      t.TempDir(),
		Capabilities: []Capability{coreToolsCapability{}},
		Plugins:      host,
		HostServices: hostmcp.Services{Secrets: wiringSecrets{}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.HasPrefix(captured["CRAFT_HOST_MCP_URL"], "http://127.0.0.1:") {
		t.Fatalf("CRAFT_HOST_MCP_URL = %q", captured["CRAFT_HOST_MCP_URL"])
	}
	if captured["CRAFT_PLUGIN_TOKEN"] == "" {
		t.Fatal("CRAFT_PLUGIN_TOKEN is empty")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
