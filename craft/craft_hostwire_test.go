package craft

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/craft/plugin"
	pluginserver "github.com/GizClaw/flowcraft/craft/plugin/server"
)

// wiringSecrets answers one host primitive, so a plugin that calls it can
// tell what craft served it.
func wiringSecrets() primitiveCapability {
	return primitiveCapability{
		name: "wiring-secrets",
		primitives: []pluginserver.Primitive{{
			Resource: "secret", Op: "get", Grant: "secrets:auth",
			Description: "Read one secret of the calling plugin",
			Params:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
			Handler: func(_ context.Context, call pluginserver.Call) (any, error) {
				if _, err := call.String("name"); err != nil {
					return nil, err
				}
				return map[string]any{"value": "value"}, nil
			},
		}},
	}
}

// wirePluginHost is one plugin host plus the paths a test needs to read what
// the plugin's process observed.
type wirePluginHost struct {
	host     *plugin.Host
	store    *plugin.Store
	dataRoot string
}

// newWirePluginHost scans one root into a store and opens a host over it.
// The wire server comes from the Craft the host is handed to: the store's
// grants are the vocabulary the test's primitives use, which is the same
// declaration an application makes.
func newWirePluginHost(
	t *testing.T,
	root string,
) *wirePluginHost {
	t.Helper()
	dataRoot := t.TempDir()
	store, err := plugin.NewStore(plugin.Options{
		Roots:       []plugin.Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: dataRoot,
		Grants:      []string{"secrets:auth"},
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	host, err := plugin.NewHost(plugin.HostOptions{Store: store})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	return &wirePluginHost{host: host, store: store, dataRoot: dataRoot}
}

// observations returns what the plugin's process recorded, in the order its
// connections were made.
func (w *wirePluginHost) observations(t *testing.T, id string) []observation {
	t.Helper()
	return readObservations(t, w.dataRoot, id)
}

// TestCraftServesHostPrimitivesOverTheWire is the wiring craft itself
// owns: a plugin process spawned by craft dials the socket craft served,
// and a primitive call it makes there reaches the host services the
// application supplied. The identity is the connection, so the plugin
// passes nothing but its request.
func TestCraftServesHostPrimitivesOverTheWire(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWirePlugin(t, root, "hello", "0.1.0", nil, "tools.provide", "secrets:auth")
	host := newWirePluginHost(t, root)
	c := newTestCraftWith(t, pluginTestDefinition, host.host, wiringSecrets())
	waitForPluginTool(t, host.host, "hello__node_echo")

	found := waitForObservations(t, host, "hello", 1)
	if got := found[0].Secret; got != "value" {
		t.Fatalf("the plugin read %q through secret.get, want the host's value", got)
	}
	if !containsString(found[0].Grants, "secrets:auth") {
		t.Fatalf("grants = %v, want the manifest's secrets:auth", found[0].Grants)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestCraftServesOnlyGrantedPrimitives pins the other half of the same
// wiring: the primitive set exists for every plugin, and the grant decides
// which of them answer. A plugin without secrets:auth reaches the endpoint
// and is refused there, with the grant named.
func TestCraftServesOnlyGrantedPrimitives(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWirePlugin(t, root, "hello", "0.1.0", nil, "tools.provide")
	host := newWirePluginHost(t, root)
	c := newTestCraftWith(t, pluginTestDefinition, host.host, wiringSecrets())
	waitForPluginTool(t, host.host, "hello__node_echo")

	found := waitForObservations(t, host, "hello", 1)
	if found[0].Secret != "" {
		t.Fatalf("a plugin without secrets:auth read %q", found[0].Secret)
	}
	if !strings.Contains(found[0].SecretError, "secrets:auth") {
		t.Fatalf("secret.get error = %q, want the missing grant named",
			found[0].SecretError)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestCraftRegrantsPerConnectionAcrossUpdateAndRollback covers what the
// connection being the identity means for a running plugin whose grants
// change. Updating to a manifest that asks for less replaces the process:
// the new connection is granted the new manifest, and the old one is gone
// with the process it belonged to. A rollback restores the wider manifest
// and grants it to the connection it opens, because a grant is something a
// connection is given, never something it keeps.
func TestCraftRegrantsPerConnectionAcrossUpdateAndRollback(t *testing.T) {
	t.Parallel()
	// The plugin is installed into the root craft scans, so a rollback has
	// a snapshot to restore: an install over a copy in the root keeps one.
	root := t.TempDir()
	host := newWirePluginHost(t, root)
	c := newTestCraftWith(t, pluginTestDefinition, host.host, wiringSecrets())
	ctx := context.Background()

	source := t.TempDir()
	wide := writeInstallableWirePlugin(
		t, source, "hello", "0.1.0", "tools.provide", "secrets:auth")
	if _, err := host.host.Install(ctx, wide); err != nil {
		t.Fatalf("Install 0.1.0: %v", err)
	}
	if err := host.host.SetEnabled(ctx, "hello", true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	started := waitForObservations(t, host, "hello", 1)[0]
	if !containsString(started.Grants, "secrets:auth") {
		t.Fatalf("the first connection's grants = %v, want the manifest's",
			started.Grants)
	}

	// The update asks for less: the tools grant stays, the secrets grant
	// goes.
	narrowed := t.TempDir()
	narrow := writeInstallableWirePlugin(
		t, narrowed, "hello", "0.2.0", "tools.provide")
	summary, err := host.host.Install(ctx, narrow)
	if err != nil || summary.Version != "0.2.0" {
		t.Fatalf("Install = %+v, %v", summary, err)
	}
	updated := waitForObservations(t, host, "hello", 2)[1]
	if containsString(updated.Grants, "secrets:auth") {
		t.Fatalf("the updated connection's grants = %v, want the narrowed "+
			"manifest's", updated.Grants)
	}
	if updated.Generation <= started.Generation {
		t.Fatalf("generations = %d then %d, want the updated process's to be newer",
			started.Generation, updated.Generation)
	}
	if !strings.Contains(updated.SecretError, "secrets:auth") {
		t.Fatalf("secret.get on the narrowed connection = %q, want a refusal",
			updated.SecretError)
	}

	restored, err := host.host.Rollback(ctx, "hello")
	if err != nil || restored.Version != "0.1.0" {
		t.Fatalf("Rollback = %+v, %v", restored, err)
	}
	regranted := waitForObservations(t, host, "hello", 3)[2]
	if !containsString(regranted.Grants, "secrets:auth") {
		t.Fatalf("the restored connection's grants = %v, want the restored "+
			"manifest's", regranted.Grants)
	}
	if regranted.Secret != "value" {
		t.Fatalf("the restored connection read %q through secret.get, want "+
			"the host's value", regranted.Secret)
	}
	entry, ok := host.store.Entry("hello")
	if !ok || entry.Manifest.Version != "0.1.0" {
		t.Fatalf("entry after the rollback = %+v, %v, want 0.1.0", entry, ok)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// waitForObservations blocks until the plugin's process recorded at least
// want connections, and returns every record it made.
func waitForObservations(
	t *testing.T,
	host *wirePluginHost,
	id string,
	want int,
) []observation {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		found := host.observations(t, id)
		if len(found) >= want {
			return found
		}
		if time.Now().After(deadline) {
			t.Fatalf("connection observations = %d, want at least %d (%+v)",
				len(found), want, found)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// containsString reports whether values holds want.
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
