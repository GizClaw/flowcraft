package craft

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/craft/hostmcp"
	"github.com/GizClaw/flowcraft/craft/plugin"
)

// secretEnvFactory is a scripted plugin source that keeps the
// environment of every process the host starts, so a test can hold the
// token one process was given after that process is gone.
type secretEnvFactory struct {
	mu   sync.Mutex
	envs []map[string]string
}

func (f *secretEnvFactory) new(
	_ context.Context,
	_ plugin.Entry,
	env map[string]string,
	_ string,
) (plugin.Source, error) {
	f.mu.Lock()
	copied := make(map[string]string, len(env))
	for key, value := range env {
		copied[key] = value
	}
	f.envs = append(f.envs, copied)
	f.mu.Unlock()
	return &mountSource{}, nil
}

func (f *secretEnvFactory) tokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.envs))
	for _, env := range f.envs {
		out = append(out, env["CRAFT_PLUGIN_TOKEN"])
	}
	return out
}

// pingHostMCP posts one MCP initialize with a plugin token and returns
// the status the endpoint answered with.
func pingHostMCP(t *testing.T, url, token string) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode
}

// TestCraftUpdateRevokesTheReplacedProcessToken covers what an update
// does to what the replaced process was allowed to do. A plugin's host
// token carries the grants of the manifest it was started from, and the
// SDK server the endpoint caches for it is built from those same grants;
// updating a running plugin to a manifest that asks for less must not
// leave the old process holding more. Both halves are pinned here: the
// token of the replaced generation stops authenticating, the cache built
// for it is dropped, and the process the update started gets a token and
// a cache of its own.
func TestCraftUpdateRevokesTheReplacedProcessToken(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeHostMCPPlugin(t, root, "hello", "0.1.0", "mcp:provide", "secrets:auth")
	store, err := plugin.NewStore(plugin.Options{
		Roots:       []plugin.Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	factory := &secretEnvFactory{}
	host, err := plugin.NewHost(plugin.HostOptions{
		Store:     store,
		NewSource: factory.new,
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	def := mustDefinition(t, pluginTestDefinition)
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
	t.Cleanup(func() { _ = c.Close() })
	tokens := factory.tokens()
	if len(tokens) != 1 || tokens[0] == "" {
		t.Fatalf("tokens of the started processes = %v, want one", tokens)
	}
	endpoint := c.hostMCP.server.URL()
	if status := pingHostMCP(t, endpoint, tokens[0]); status != http.StatusOK {
		t.Fatalf("the running process's token answered %d, want 200", status)
	}
	if !c.hostMCP.server.Cached("hello") {
		t.Fatal("the endpoint did not cache a server for the calling plugin")
	}

	// The update asks for less: the server stays, the secrets grant goes.
	source := t.TempDir()
	writeHostMCPPlugin(t, source, "hello", "0.2.0", "mcp:provide")
	summary, err := host.Install(context.Background(), filepath.Join(source, "hello"))
	if err != nil || summary.Version != "0.2.0" {
		t.Fatalf("Install = %+v, %v", summary, err)
	}
	updated := factory.tokens()
	if len(updated) != 2 || updated[1] == "" || updated[1] == tokens[0] {
		t.Fatalf("tokens = %v, want a new one for the updated process", updated)
	}
	if status := pingHostMCP(t, endpoint, tokens[0]); status != http.StatusUnauthorized {
		t.Fatalf("the replaced process's token answered %d, want 401", status)
	}
	if c.hostMCP.server.Cached("hello") {
		t.Fatal("the SDK server of the replaced process is still cached")
	}
	if status := pingHostMCP(t, endpoint, updated[1]); status != http.StatusOK {
		t.Fatalf("the updated process's token answered %d, want 200", status)
	}
	identity, _, ok := c.hostMCP.tokens.Lookup(updated[1])
	if !ok || identity.Grants.Has("secrets:auth") {
		t.Fatalf("the updated process's grants = %+v, want the new manifest's", identity)
	}
}

// TestCraftRollbackRegrantsTheRestoredVersion covers what a rollback to
// a version that declares more permissions grants the plugin again.
// Grants belong to a process: the restored version is the manifest the
// record names again, and it is authorized by being started — the
// process that was running before the rollback keeps nothing, because
// its grants were the narrower manifest's and its generation is gone.
// A plugin that asks for more than the version it replaces cannot
// inherit it from the process in place, which is what makes the record
// and the process agree on the permissions in force either way.
func TestCraftRollbackRegrantsTheRestoredVersion(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// The version on disk at start declares the wider set and the update
	// narrows it, so the rollback restores a plugin asking for more than
	// the process it replaces.
	writeHostMCPPlugin(t, root, "hello", "0.1.0", "mcp:provide", "secrets:auth")
	store, err := plugin.NewStore(plugin.Options{
		Roots:       []plugin.Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	factory := &secretEnvFactory{}
	host, err := plugin.NewHost(plugin.HostOptions{
		Store:     store,
		NewSource: factory.new,
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	def := mustDefinition(t, pluginTestDefinition)
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
	t.Cleanup(func() { _ = c.Close() })
	endpoint := c.hostMCP.server.URL()

	tokens := factory.tokens()
	if len(tokens) != 1 || tokens[0] == "" {
		t.Fatalf("tokens of the started processes = %v, want one", tokens)
	}
	if identity, _, ok := c.hostMCP.tokens.Lookup(tokens[0]); !ok ||
		!identity.Grants.Has("secrets:auth") {
		t.Fatalf("the started process's grants = %+v, want the manifest's", identity)
	}

	// The update asks for less: the server stays, the secrets grant goes.
	source := t.TempDir()
	writeHostMCPPlugin(t, source, "hello", "0.2.0", "mcp:provide")
	summary, err := host.Install(context.Background(), filepath.Join(source, "hello"))
	if err != nil || summary.Version != "0.2.0" {
		t.Fatalf("Install = %+v, %v", summary, err)
	}
	narrowed := factory.tokens()
	if len(narrowed) != 2 || narrowed[1] == "" || narrowed[1] == tokens[0] {
		t.Fatalf("tokens = %v, want a new one for the updated process", narrowed)
	}
	if identity, _, ok := c.hostMCP.tokens.Lookup(narrowed[1]); !ok ||
		identity.Grants.Has("secrets:auth") {
		t.Fatalf("the updated process's grants = %+v, want the narrowed manifest's", identity)
	}
	// One ping builds the endpoint's cached SDK server for the calling
	// process, so the rollback has a cache to drop.
	if status := pingHostMCP(t, endpoint, narrowed[1]); status != http.StatusOK {
		t.Fatalf("the updated process's token answered %d, want 200", status)
	}

	restored, err := host.Rollback(context.Background(), "hello")
	if err != nil || restored.Version != "0.1.0" {
		t.Fatalf("Rollback = %+v, %v", restored, err)
	}
	tokens = factory.tokens()
	if len(tokens) != 3 || tokens[2] == "" || tokens[2] == narrowed[1] {
		t.Fatalf("tokens = %v, want a new one for the restored process", tokens)
	}
	if status := pingHostMCP(t, endpoint, narrowed[1]); status != http.StatusUnauthorized {
		t.Fatalf("the replaced process's token answered %d, want 401", status)
	}
	if status := pingHostMCP(t, endpoint, tokens[2]); status != http.StatusOK {
		t.Fatalf("the restored process's token answered %d, want 200", status)
	}
	identity, _, ok := c.hostMCP.tokens.Lookup(tokens[2])
	if !ok || !identity.Grants.Has("secrets:auth") {
		t.Fatalf("the restored process's grants = %+v, want the restored manifest's",
			identity)
	}
	entry, ok := store.Entry("hello")
	if !ok || entry.Manifest.Version != "0.1.0" {
		t.Fatalf("entry after the rollback = %+v, %v, want 0.1.0", entry, ok)
	}
}

// writeHostMCPPlugin writes one plugin manifest declaring an MCP server
// and the given permissions.
func writeHostMCPPlugin(
	t *testing.T,
	root, id, version string,
	permissions ...string,
) {
	t.Helper()
	quoted := make([]string, 0, len(permissions))
	for _, permission := range permissions {
		quoted = append(quoted, `"`+permission+`"`)
	}
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifest := `{"id": "` + id + `", "version": "` + version + `",
		"permissions": [` + strings.Join(quoted, ", ") + `],
		"mcp": {"command": "true"}}`
	if err := os.WriteFile(
		filepath.Join(dir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("manifest: %v", err)
	}
}

// pluginTestDefinition is the smallest deployable definition a Craft
// starts from.
const pluginTestDefinition = `
craft:
  id: test
  version: 0.1.0
deploy:
  version: v1
  resources:
    bus: {kind: event.Bus, impl: memory}
  runtime:
    event_bus: bus
`
