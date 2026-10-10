package plugin

import (
	"context"
	"strings"
	"testing"
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
// rewrites "." — cannot both be valid, and the one that lost the race is
// reported with the plugin it collides with instead of silently losing
// every tool it publishes.
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
	if len(entries) != 1 || entries[0].ID != "hello.world" {
		t.Fatalf("valid plugins = %v, want only the first to hold the namespace",
			entries)
	}
	if _, ok := store.Entry("hello_world"); ok {
		t.Fatal("a plugin whose namespace is taken is still valid")
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
		if !strings.Contains(summary.Error,
			`the tool namespace "hello_world__" collides with plugin "hello.world"`) {
			t.Fatalf("collision error = %q", summary.Error)
		}
	}
	if !found {
		t.Fatal("the colliding plugin is not listed at all")
	}
}
