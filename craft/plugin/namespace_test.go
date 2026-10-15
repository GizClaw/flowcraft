package plugin

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// TestToolNamespaces pins the namespace of every MCP server of one
// plugin: the plugin's own namespace for a single server, and the plugin
// plus the server for a manifest that declares several, so two servers
// of one manifest cannot publish the same tool name and lose one of
// them to the registry's duplicate rule.
func TestToolNamespaces(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		id      string
		servers []MCPServer
		want    []string
	}{
		{
			name:    "a single server keeps the plugin namespace",
			id:      "hello",
			servers: []MCPServer{{Name: "primary", Command: "true"}},
			want:    []string{"hello__"},
		},
		{
			name:    "no server publishes no namespace",
			id:      "hello",
			servers: nil,
			want:    nil,
		},
		{
			name: "several servers add their names",
			id:   "hello",
			servers: []MCPServer{
				{Name: "read", Command: "true"},
				{Name: "write", Command: "true"},
			},
			want: []string{"hello__read__", "hello__write__"},
		},
		{
			name:    "an unnamed server falls back to its position",
			id:      "hello",
			servers: []MCPServer{{Command: "true"}, {Name: " write ", Command: "true"}},
			want:    []string{"hello__1__", "hello__write__"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got := ToolNamespaces(testCase.id, testCase.servers)
			if len(got) != len(testCase.want) {
				t.Fatalf("ToolNamespaces = %v, want %v", got, testCase.want)
			}
			for i := range got {
				if got[i] != testCase.want[i] {
					t.Fatalf("ToolNamespaces = %v, want %v", got, testCase.want)
				}
			}
		})
	}
}

// TestManifestRejectsServersSharingANamespace covers the manifest half
// of the rule: when two of a plugin's own servers collapse into one
// namespace the manifest is invalid, because nothing downstream could
// tell which server a tool name belongs to.
func TestManifestRejectsServersSharingANamespace(t *testing.T) {
	t.Parallel()
	manifest, err := ParseManifest(context.Background(), []byte(`{
		"id": "hello", "version": "0.1.0",
		"permissions": ["mcp:provide"],
		"mcpServers": [
			{"name": "tools.one", "command": "true"},
			{"name": "tools_one", "command": "true"}
		]
	}`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	err = manifest.Validate("")
	if err == nil || !strings.Contains(err.Error(), "share the tool namespace") {
		t.Fatalf("Validate = %v, want the shared namespace to be refused", err)
	}
	if !strings.Contains(err.Error(), `"hello__tools_one__"`) {
		t.Fatalf("Validate = %v, want it to name the shared namespace", err)
	}
}

// TestStoreMarksToolNamespaceCollisions covers the store half: two
// plugins whose ids collapse into one tool namespace — ToolPrefix
// rewrites "." — cannot both be published, and the one that lost the
// race is told which plugin it collides with instead of silently losing
// every tool it publishes.
//
// The collision is not an invalid manifest: the plugin is listed, it is
// readable, and it can be disabled — which is the recovery, because the
// collision is over as soon as one of the two stops claiming the
// namespace. Only enabling it is refused.
func TestStoreMarksToolNamespaceCollisions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "first", `{
		"id": "hello.world", "version": "0.1.0",
		"permissions": ["mcp:provide"], "mcp": {"command": "true"}
	}`)
	writePlugin(t, root, "second", `{
		"id": "hello_world", "version": "0.2.0",
		"permissions": ["mcp:provide"], "mcp": {"command": "true"}
	}`)
	store := newStoreOver(t, root)

	entries, err := store.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("valid plugins = %v, want both listed", entries)
	}
	collided, ok := store.Entry("hello_world")
	if !ok {
		t.Fatal("a plugin whose namespace is taken is not readable")
	}
	const want = `the tool namespace "hello_world__" collides with plugin "hello.world"`
	if collided.Conflict != want {
		t.Fatalf("conflict = %q, want %q", collided.Conflict, want)
	}
	if collided.Error != "" {
		t.Fatalf("error = %q, want a collision to be reported as a conflict", collided.Error)
	}
	// It is not started, and it cannot be enabled while the collision
	// lasts.
	enabled, err := store.Enabled()
	if err != nil {
		t.Fatalf("Enabled: %v", err)
	}
	for _, entry := range enabled {
		if entry.ID == "hello_world" {
			t.Fatal("a plugin whose namespace is taken was listed as startable")
		}
	}
	if err := store.SetEnabled("hello_world", true); !errdefs.IsConflict(err) {
		t.Fatalf("enabling the collided plugin = %v, want Conflict", err)
	}
	// Disabling it is the way out of the collision.
	if err := store.SetEnabled("hello_world", false); err != nil {
		t.Fatalf("disabling the collided plugin: %v", err)
	}
	if got, ok := store.Entry("hello_world"); !ok || got.Enabled {
		t.Fatalf("entry after the disable = %+v, %v", got, ok)
	}
	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, summary := range list {
		if summary.ID != "hello_world" {
			continue
		}
		found = true
		if summary.Conflict != want {
			t.Fatalf("collision = %q, want %q", summary.Conflict, want)
		}
		if summary.Error != "" {
			t.Fatalf("error = %q, want the entry to be valid", summary.Error)
		}
	}
	if !found {
		t.Fatal("the colliding plugin is not listed at all")
	}
}
