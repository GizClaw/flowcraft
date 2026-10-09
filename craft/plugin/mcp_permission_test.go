package plugin

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/tool"
)

// sourceFactory is a scripted NewSourceFunc: it records which plugins
// reached the factory and answers direct calls with the tool name it
// was given, so a test can tell routing from refusal. The factory
// stands in for the child process: a plugin it was never asked about
// never got one.
type sourceFactory struct {
	created map[string]int
	closed  map[string]int
}

func newSourceFactory() *sourceFactory {
	return &sourceFactory{
		created: map[string]int{},
		closed:  map[string]int{},
	}
}

func (f *sourceFactory) new(
	_ context.Context,
	entry Entry,
	_ map[string]string,
	_ string,
) (Source, error) {
	f.created[entry.ID]++
	source := &fakeSource{
		tools: []tool.Tool{fakeTool{name: entry.ID + "__echo"}},
		call: func(toolName string, _ any) (json.RawMessage, error) {
			return json.RawMessage(`{"writes":{"result":"` + toolName + `"}}`), nil
		},
	}
	source.close = func() { f.closed[entry.ID]++ }
	return source, nil
}

// newStoreOver scans one plugin root into a fresh store.
func newStoreOver(t *testing.T, root string) *Store {
	t.Helper()
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

// startHost starts a host over a store with the given source factory,
// or with the production MCP source when it is nil.
func startHost(t *testing.T, store *Store, newSource NewSourceFunc) *Host {
	t.Helper()
	host, err := NewHost(HostOptions{Store: store, NewSource: newSource})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	if err := host.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	return host
}

// TestMCPSectionRequiresPermission covers the mcp:provide grant, whose
// checkpoint is synthesis: a plugin that declares an mcp section
// without the permission gets no server at all. No child process is
// started, no tool is published, and a direct caller — a plugin graph
// node — is told which grant is missing rather than that the server
// does not exist.
func TestMCPSectionRequiresPermission(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const grantedID = "granted"
	const ungrantedID = "ungranted"
	writePlugin(t, root, grantedID, contributionManifest(grantedID, "mcp:provide"))
	writePlugin(t, root, ungrantedID, contributionManifest(ungrantedID))
	for _, dir := range []string{grantedID, ungrantedID} {
		writeContributionTree(t, root, dir)
	}
	factory := newSourceFactory()
	host := startHost(t, newStoreOver(t, root), factory.new)
	ctx := context.Background()

	if reached := factory.created[ungrantedID]; reached != 0 {
		t.Fatalf("the ungranted plugin reached the source factory %d times: "+
			"its mcp section was not ignored", reached)
	}
	if reached := factory.created[grantedID]; reached != 1 {
		t.Fatalf("the granted plugin reached the source factory %d times, want 1",
			reached)
	}

	names := []string{}
	for _, candidate := range host.ToolSet().Tools() {
		names = append(names, candidate.Definition().Name)
	}
	if !slices.Equal(names, []string{grantedID + "__echo"}) {
		t.Fatalf("published tools = %v, want only the granted plugin's", names)
	}

	_, err := host.CallTool(ctx, ungrantedID, "node_echo", nil)
	if !errdefs.IsForbidden(err) {
		t.Fatalf("CallTool without mcp:provide = %v, want Forbidden", err)
	}
	if !strings.Contains(err.Error(), "mcp:provide") {
		t.Fatalf("CallTool error = %v, want it to name the missing permission", err)
	}

	out, err := host.CallTool(ctx, grantedID, "node_echo", nil)
	if err != nil {
		t.Fatalf("CallTool of the granted plugin: %v", err)
	}
	if string(out) != `{"writes":{"result":"node_echo"}}` {
		t.Fatalf("CallTool result = %s, want the plugin's own answer", out)
	}
}

// TestPluginWithoutMCPSectionStarts pins the boundary of the gate: the
// refusal covers a declared mcp section, not the absence of the grant
// on its own. A plugin that ships no server has nothing to ignore, so
// it keeps the empty source — and therefore the missing-server error
// of a direct call — it always had.
func TestPluginWithoutMCPSectionStarts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const id = "skills"
	writePlugin(t, root, id, `{
		"id": "skills", "version": "0.1.0",
		"permissions": ["skills:provide"],
		"skills": ["skills"]
	}`)
	writeContributionTree(t, root, id)
	host := startHost(t, newStoreOver(t, root), nil)

	_, err := host.CallTool(context.Background(), id, "node_echo", nil)
	if !errdefs.IsNotFound(err) {
		t.Fatalf("CallTool = %v, want NotFound", err)
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("CallTool error = %v, want the missing-server error", err)
	}
	if tools := host.ToolSet().Tools(); len(tools) != 0 {
		t.Fatalf("tools of a server-less plugin = %v, want none", tools)
	}
}
