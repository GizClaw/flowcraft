package plugin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/tool"
)

// sourceFactory is a scripted NewSourceFunc: it records which plugins
// reached the factory and answers direct calls with the tool name it
// was given, so a test can tell routing from refusal. The factory
// stands in for the plugin process: a plugin it was never asked about
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
	_ uint64,
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
// or with the production wire source when it is nil. A nil server is
// filled in: a source that is expected to spawn a process needs a socket
// to spawn it against, and a test that only scripts a factory never
// reaches one.
func startHost(t *testing.T, store *Store, newSource NewSourceFunc) *Host {
	t.Helper()
	opts := HostOptions{Store: store, NewSource: newSource}
	if newSource == nil {
		opts.Server = newWireServer(t)
	}
	host, err := NewHost(opts)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	if err := host.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	return host
}

// TestToolsProvideGatesTheDeclaration covers the tools:provide grant,
// whose checkpoint is projection: a plugin that runs without the
// permission still starts — it may contribute skills or hooks — but none
// of the tools it declares are published, and a direct caller is told
// which grant is missing rather than that the tool does not exist.
func TestToolsProvideGatesTheDeclaration(t *testing.T) {
	root := t.TempDir()
	const grantedID = "granted"
	const ungrantedID = "ungranted"
	writePlugin(t, root, grantedID, wireManifest(grantedID, "tools:provide"))
	writePlugin(t, root, ungrantedID, wireManifest(ungrantedID))
	for _, dir := range []string{grantedID, ungrantedID} {
		writeContributionTree(t, root, dir)
		writeHelperCommand(t, filepath.Join(root, dir))
	}
	host := startHost(t, newStoreOver(t, root), nil)
	ctx := context.Background()

	if !running(host, grantedID) || !running(host, ungrantedID) {
		t.Fatalf("the plugins did not both start: %v", toolNames(host))
	}
	// A start does not wait for the wire handshake, so the granted
	// plugin's declaration arrives a moment after the process does.
	waitForTool(t, host, grantedID+"__echo")
	names := toolNames(host)
	if !slices.Contains(names, grantedID+"__echo") {
		t.Fatalf("published tools = %v, want the granted plugin's", names)
	}
	for _, name := range names {
		if strings.HasPrefix(name, ungrantedID+"__") {
			t.Fatalf("published tools = %v, want none of the ungranted plugin's", names)
		}
	}

	_, err := host.CallTool(ctx, ungrantedID, "echo", nil)
	if !errdefs.IsForbidden(err) {
		t.Fatalf("CallTool without tools:provide = %v, want Forbidden", err)
	}
	if !strings.Contains(err.Error(), "tools:provide") {
		t.Fatalf("CallTool error = %v, want it to name the missing permission", err)
	}

	out, err := host.CallTool(ctx, grantedID, "node_echo", nil)
	if err != nil {
		t.Fatalf("CallTool of the granted plugin: %v", err)
	}
	if string(out) != `{"writes":{"result":"echo:"}}` {
		t.Fatalf("CallTool result = %s, want the plugin's own answer", out)
	}
}

// TestPluginWithoutWireStarts pins the boundary of the gate: the refusal
// covers a declared tool set, not the absence of the grant on its own. A
// plugin that ships no run section has nothing to open, so it keeps the
// empty source — and therefore the no-tools error of a direct call — it
// always had.
func TestPluginWithoutWireStarts(t *testing.T) {
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
	if !errdefs.IsForbidden(err) {
		t.Fatalf("CallTool = %v, want Forbidden", err)
	}
	if !strings.Contains(err.Error(), "no run section") {
		t.Fatalf("CallTool error = %v, want the no-run error", err)
	}
	if tools := host.ToolSet().Tools(); len(tools) != 0 {
		t.Fatalf("tools of a wire-less plugin = %v, want none", tools)
	}
}
