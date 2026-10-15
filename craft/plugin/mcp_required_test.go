package plugin

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// TestMCPSourceMarksRequiredServers pins the manifest's word reaching
// the layer that documents it. Core's [mcp.WithRequired] says a host
// that cannot start without a server marks it required and then awaits
// readiness — which is what an enable does — and the give-up line is
// where the marking is visible: it carries required=true for a server
// the manifest declared and nothing at all for one it did not. Dropping
// the declaration on the way there would leave plugin.json saying one
// thing and the MCP layer reporting another.
func TestMCPSourceMarksRequiredServers(t *testing.T) {
	sink := installLogSink(t)
	host, err := NewHost(HostOptions{Store: newStoreOver(t, t.TempDir())})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}

	// Both servers name a command that cannot be started, which is the
	// terminal failure the give-up line reports; they differ only in
	// what their manifests declare.
	for _, declare := range []struct {
		id       string
		required bool
	}{
		{id: "needed", required: true},
		{id: "optional", required: false},
	} {
		entry := Entry{
			ID:  declare.id,
			Dir: t.TempDir(),
			Manifest: Manifest{
				ID:      declare.id,
				Version: "0.1.0",
				MCP: &MCPServer{
					Command:  "server/missing-binary",
					Required: declare.required,
				},
			},
		}
		source, err := host.newSource(
			context.Background(), entry, map[string]string{}, t.TempDir())
		if err != nil {
			t.Fatalf("open %s: %v", declare.id, err)
		}
		records := waitForGiveUp(t, sink, declare.id)
		if err := source.Close(); err != nil {
			t.Fatalf("close %s: %v", declare.id, err)
		}
		_, marked := records.attrs["required"]
		if marked != declare.required {
			t.Fatalf("server %s: give-up line required=%q (present=%v), want declared=%v",
				declare.id, records.attrs["required"], marked, declare.required)
		}
	}
}

// waitForGiveUp waits for the terminal give-up of one server, which the
// source reports from its retry goroutine.
func waitForGiveUp(t *testing.T, sink *logSink, server string) logRecord {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, record := range sink.find("giving up") {
			if record.attrs["server"] == server {
				return record
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no give-up reported for server %s", server)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHostEnableRefusesAnUnstartableServerEitherWay pins what the
// manifest's required flag does — and does not do — to an enable. The
// flag reaches the MCP layer (see [TestMCPSourceMarksRequiredServers]);
// it does not change what an enable does with a server whose command the
// OS cannot start. An enable waits for every server its manifest
// declares, because "recorded enabled" means this plugin's process is up
// and serving: a plugin that came up with half of what it declares
// missing is the disagreement the wait exists to prevent. Both
// declarations are refused the same way — the failure is reported, the
// process is stopped, and the record is not left saying enabled about a
// plugin that has no process.
func TestHostEnableRefusesAnUnstartableServerEitherWay(t *testing.T) {
	for _, declare := range []struct {
		name     string
		required bool
	}{
		{name: "required", required: true},
		{name: "optional", required: false},
	} {
		t.Run(declare.name, func(t *testing.T) {
			t.Parallel()
			const id = "hello"
			root := t.TempDir()
			writePlugin(t, root, id, `{"id": "`+id+`", "name": "Hello",
				"version": "0.1.0", "permissions": ["mcp:provide"],
				"mcp": {"command": "flowcraft-no-such-plugin-server",
				        "required": `+strconv.FormatBool(declare.required)+`}}`)
			store := newStoreOver(t, root)
			if err := store.SetEnabled(id, false); err != nil {
				t.Fatalf("record the plugin disabled: %v", err)
			}
			host := startHost(t, store, nil)
			host.enableTimeout = 5 * time.Second

			err := host.SetEnabled(context.Background(), id, true)
			if !errdefs.IsValidation(err) {
				t.Fatalf("enable of an unstartable command = %v, want Validation", err)
			}
			entry, ok := store.Entry(id)
			if !ok {
				t.Fatal("the plugin left the store")
			}
			if entry.Enabled {
				t.Fatal("a plugin with no process was left recorded enabled")
			}
			if host.isLive(id) {
				t.Fatal("the refused enable left a process behind")
			}
		})
	}
}
