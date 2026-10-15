package plugin

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// The tests here cover plugins that declare several MCP servers: the
// adapter's own claims about them (which names the host attaches, that
// one bound covers all of them, what it answers with) and the promise the
// wait makes about them end to end.

// multiServerManifest is a manifest whose MCP section declares one server
// per argument, in order.
func multiServerManifest(servers ...string) string {
	return `{
		"id": "hello", "version": "0.1.0",
		"permissions": ["mcp:provide"],
		"mcpServers": [` + strings.Join(servers, ",") + `]
	}`
}

// helperServer is one stdio server pointing at this test binary's stop
// helper: it comes up and serves its echo tool.
func helperServer() string {
	return `{"command": "./server",` +
		`"args": ["-test.run=TestPluginStopHelper"],` +
		`"env": {"CRAFT_PLUGIN_STOP_HELPER": "1"}}`
}

// unreachableServer is one remote server nothing is listening for: it is
// never ready, and it never gives up either, so waiting on it is what
// runs a bound out.
func unreachableServer() string {
	return `{"transport": "http", "url": "http://127.0.0.1:1/mcp"}`
}

// mcpChildOf returns the adapter the host recorded for one plugin.
func mcpChildOf(t *testing.T, host *Host, id string) *mcpChild {
	t.Helper()
	host.mu.Lock()
	source := host.live[id].source
	host.mu.Unlock()
	child, ok := source.(*mcpChild)
	if !ok {
		t.Fatalf("the host recorded %T for %s, want an mcpChild", source, id)
	}
	return child
}

// TestHostEnableWaitsForEveryServer covers a plugin that declares several
// servers: the host attaches one per declaration — the plugin id, then
// id-2 — and the enable is up only when every one of them is, not when
// the first one is.
func TestHostEnableWaitsForEveryServer(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "hello", multiServerManifest(helperServer(), helperServer()))
	writeHelperCommand(t, filepath.Join(root, "hello"))
	plugin := realPluginHost(t, root, HostOptions{})

	if err := plugin.host.SetEnabled(context.Background(), "hello", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got := mcpChildOf(t, plugin.host, "hello").servers; !slices.Equal(got,
		[]string{"hello", "hello-2"}) {
		t.Fatalf("servers = %v, want [hello hello-2]", got)
	}
	// Both servers are serving when the enable returns, so both
	// projections are published.
	got := toolNames(plugin.host)
	for _, want := range []string{"hello__1__echo", "hello__2__echo"} {
		if !slices.Contains(got, want) {
			t.Fatalf("tools = %v, want %s", got, want)
		}
	}
}

// TestHostEnableSharesOneBoundAcrossServers pins the bound the servers of
// one plugin share: they are waited on under one deadline rather than one
// each, so a plugin whose servers are all down does not hold the enable
// open for a multiple of the timeout.
func TestHostEnableSharesOneBoundAcrossServers(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "hello", multiServerManifest(
		unreachableServer(), unreachableServer(), unreachableServer()))
	bound := 500 * time.Millisecond
	plugin := realPluginHost(t, root, HostOptions{EnableTimeout: bound})

	start := time.Now()
	if err := plugin.host.SetEnabled(context.Background(), "hello", true); err != nil {
		t.Fatalf("enable of unreachable servers: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < bound {
		t.Fatalf("enable returned in %s, before the bound it was given", elapsed)
	}
	if elapsed > 2*bound {
		t.Fatalf("enable took %s for three servers, want one bound of %s",
			elapsed, bound)
	}
	// Servers that are not there yet are not a failure of enable.
	if !enabled(t, plugin.store, "hello") {
		t.Fatal("the entry was not recorded enabled")
	}
	if !running(plugin.host, "hello") {
		t.Fatal("the source was dropped instead of left reconnecting")
	}
}

// TestMCPChildWaitReadyJoinsServersThatAreNotUpYet pins the answer the
// adapter gives when none of its servers is up yet: every one of them is
// waited on and their failures arrive together as one non-terminal error
// — the "not yet" an enable records enabled on — rather than one server
// at a time.
func TestMCPChildWaitReadyJoinsServersThatAreNotUpYet(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "hello", multiServerManifest(
		unreachableServer(), unreachableServer()))
	plugin := realPluginHost(t, root, HostOptions{})
	entry, ok := plugin.store.Entry("hello")
	if !ok {
		t.Fatal("the plugin is not in the store")
	}
	source, err := plugin.host.mcpSource(
		context.Background(), entry, nil, t.TempDir())
	if err != nil {
		t.Fatalf("mcpSource: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })
	child, ok := source.(*mcpChild)
	if !ok {
		t.Fatalf("mcpSource returned %T, want an mcpChild", source)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = child.WaitReady(ctx, 100*time.Millisecond)
	if err == nil {
		t.Fatal("WaitReady of servers that never come up = nil")
	}
	if errdefs.IsValidation(err) {
		t.Fatalf("WaitReady = %v, want the not-yet class", err)
	}
	for _, want := range []string{`server "hello"`, `server "hello-2"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("WaitReady error = %v, want %s named", err, want)
		}
	}
}
