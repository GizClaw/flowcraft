package craft

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	coregraph "github.com/GizClaw/flowcraft/core/graph"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/craft/plugin"
)

// engineDeps records what the test engine factory was handed, so tests
// can assert that a mount reached the agent engine and not only the
// deployment document.
type engineDeps struct {
	mu   sync.Mutex
	deps map[string]any
}

func (d *engineDeps) record(deps map[string]any) {
	d.mu.Lock()
	d.deps = deps
	d.mu.Unlock()
}

func (d *engineDeps) dep(name string) (any, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	value, ok := d.deps[name]
	return value, ok
}

// names returns the dependency names the engine was built with.
func (d *engineDeps) names() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.deps))
	for name := range d.deps {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

type nodeEngineCapability struct{ seen *engineDeps }

func (nodeEngineCapability) Name() string { return "node-engine" }

func (c nodeEngineCapability) Register(registry *resource.Registry) error {
	if err := event.Register(registry); err != nil {
		return err
	}
	return registry.Register(nodeEngineFactory(c))
}

type nodeEngineFactory struct{ seen *engineDeps }

func (nodeEngineFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "agent.Engine",
		Impl: "test",
		Deps: []resource.DepSpec{{
			Name: "node_type", Type: "graph.NodeTypeRegistrar", Many: true,
		}},
	}
}

func (f nodeEngineFactory) New(_ context.Context, in resource.Input) (any, error) {
	if f.seen != nil {
		f.seen.record(in.Deps)
	}
	return agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		return board, nil
	}), nil
}

// nodeHost is a PluginHost exposing one plugin that declares the echo
// node, the given permissions, and a scriptable tool result.
type nodeHost struct {
	permissions []string
	nodes       []plugin.NodeDecl
	reply       func(pluginID, toolName string, args any) (json.RawMessage, error)
	calls       []string
	args        []any
}

// newNodeHost builds a plugin host whose single plugin declares
// nodes:provide, mcp:provide plus permissions, and the default echo
// node. A node plugin needs both grants: the node declaration is gated
// by nodes:provide, and the tool it calls lives on the plugin's own
// MCP server, which mcp:provide covers.
func newNodeHost(permissions ...string) *nodeHost {
	if len(permissions) == 0 {
		permissions = []string{"nodes:provide", "mcp:provide"}
	}
	return &nodeHost{
		permissions: permissions,
		nodes: []plugin.NodeDecl{{
			Type: "echo", Tool: "node_echo", Timeout: "5s",
		}},
	}
}

func (h *nodeHost) Start(context.Context) error { return nil }
func (h *nodeHost) Close() error                { return nil }
func (h *nodeHost) Revision() uint64            { return 1 }
func (h *nodeHost) Subscribe(func()) func()     { return func() {} }
func (h *nodeHost) Tools() tool.Source          { return plugin.NewToolSet() }
func (h *nodeHost) SkillRoots() []string        { return nil }
func (h *nodeHost) HookFiles() []string         { return nil }

func (h *nodeHost) Entries() ([]plugin.Entry, error) {
	return []plugin.Entry{{
		ID: "hello",
		Manifest: plugin.Manifest{
			ID:          "hello",
			Version:     "0.1.0",
			Permissions: h.permissions,
			Nodes:       h.nodes,
		},
	}}, nil
}

func (h *nodeHost) CallTool(
	_ context.Context,
	pluginID, toolName string,
	args any,
) (json.RawMessage, error) {
	h.calls = append(h.calls, pluginID+"."+toolName)
	h.args = append(h.args, args)
	if h.reply != nil {
		return h.reply(pluginID, toolName, args)
	}
	return json.RawMessage(`{"writes":{"result":"hello"}}`), nil
}

// nodeDefinition builds a craft definition mounting plugin nodes into
// the named agents.
func nodeDefinition(targets string) string {
	return `
craft: {id: test, version: 0.1.0}
plugins: {node_targets: [` + targets + `]}
deploy:
  version: v1
  resources:
    bus: {kind: event.Bus, impl: memory}
  agents:
    bot:
      card: {name: Bot}
      engine: {kind: agent.Engine, impl: test}
  runtime:
    event_bus: bus
`
}

// newTestCraft builds and starts a Craft over a node definition.
func newTestCraft(t *testing.T, definition string, host PluginHost) *Craft {
	t.Helper()
	def, err := ParseDefinition([]byte(definition))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	c, err := New(def, Options{
		DataDir:      t.TempDir(),
		Capabilities: []Capability{nodeEngineCapability{}},
		Plugins:      host,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestCraftMountsPluginNodeTypes covers the whole mount: manifest scan
// → synthesized graph.NodeType resource → agent engine node_type
// dependency → a registrar the graph kernel can register.
func TestCraftMountsPluginNodeTypes(t *testing.T) {
	t.Parallel()
	seen := &engineDeps{}
	def, err := ParseDefinition([]byte(nodeDefinition("bot")))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	c, err := New(def, Options{
		DataDir:      t.TempDir(),
		Capabilities: []Capability{nodeEngineCapability{seen: seen}},
		Plugins:      newNodeHost(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	// The composed document carries the synthesized resource and the
	// engine dependency that points at it.
	doc, _, _, err := c.compose(ctx, DefaultKey, RuntimeOptions{})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	synthesized, ok := doc.Resources["plugin.hello.node.echo"]
	if !ok {
		t.Fatalf("synthesized resource missing; resources = %v",
			slices.Sorted(maps.Keys(doc.Resources)))
	}
	var settings pluginNodeSettings
	if err := json.Unmarshal(synthesized.Settings, &settings); err != nil {
		t.Fatalf("decode settings %s: %v", synthesized.Settings, err)
	}
	want := pluginNodeSettings{
		Type: "hello.echo", Plugin: "hello", Tool: "node_echo", Timeout: "5s",
	}
	if settings != want {
		t.Fatalf("settings = %+v, want %+v", settings, want)
	}
	if synthesized.Kind != pluginNodeTypeKind || synthesized.Impl != pluginNodeTypeImpl {
		t.Fatalf("synthesized resource = %s/%s, want %s/%s",
			synthesized.Kind, synthesized.Impl, pluginNodeTypeKind, pluginNodeTypeImpl)
	}
	dep := doc.Agents["bot"].Engine.Deps["node_type.hello.echo"]
	if dep != resource.Ref("plugin.hello.node.echo") {
		t.Fatalf("agent engine dep node_type.hello.echo = %q, want the synthesized resource", dep)
	}

	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rt, err := c.OpenRuntime(ctx, DefaultKey, RuntimeOptions{})
	if err != nil {
		t.Fatalf("OpenRuntime: %v", err)
	}
	value, ok := rt.Resource("plugin.hello.node.echo")
	if !ok {
		t.Fatal("synthesized node type resource missing from the runtime")
	}
	registrar, ok := value.(pluginNodeRegistrar)
	if !ok {
		t.Fatalf("resource value = %T, want a node type registrar", value)
	}
	if registrar.name != "hello.echo" {
		t.Fatalf("registrar name = %q, want hello.echo", registrar.name)
	}

	// The engine was handed the dependency: this is the assertion the
	// resource-existence check alone does not make.
	injected, ok := seen.dep("node_type.hello.echo")
	if !ok {
		t.Fatalf("engine deps = %v, want node_type.hello.echo", seen.names())
	}
	if injectedRegistry, ok := injected.(pluginNodeRegistrar); !ok ||
		injectedRegistry.name != "hello.echo" {
		t.Fatalf("engine node_type.hello.echo = %#v, want the hello.echo registrar", injected)
	}

	// And the registrar produces a node type the graph kernel accepts.
	registry := coregraph.NewRegistry()
	if err := registrar.Register(registry); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !registry.Has("hello.echo") {
		t.Fatalf("registry = %v, want hello.echo", registry.TypeNames())
	}
}

// TestPluginNodesRequirePermission is the fail-closed half of the
// nodes:provide grant: without it the plugin contributes nothing —
// no resource, no engine dependency.
func TestPluginNodesRequirePermission(t *testing.T) {
	t.Parallel()
	seen := &engineDeps{}
	c, err := New(mustDefinition(t, nodeDefinition("bot")), Options{
		DataDir:      t.TempDir(),
		Capabilities: []Capability{nodeEngineCapability{seen: seen}},
		Plugins:      newNodeHost("mcp:provide"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	doc, _, _, err := c.compose(ctx, DefaultKey, RuntimeOptions{})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if _, ok := doc.Resources["plugin.hello.node.echo"]; ok {
		t.Fatal("node resource synthesized without nodes:provide")
	}
	if dep := doc.Agents["bot"].Engine.Deps["node_type.hello.echo"]; dep != "" {
		t.Fatalf("engine dep = %q, want none", dep)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rt, err := c.OpenRuntime(ctx, DefaultKey, RuntimeOptions{})
	if err != nil {
		t.Fatalf("OpenRuntime: %v", err)
	}
	if _, ok := rt.Resource("plugin.hello.node.echo"); ok {
		t.Fatal("node resource present in the runtime without nodes:provide")
	}
	if names := seen.names(); len(names) != 0 {
		t.Fatalf("engine deps = %v, want none", names)
	}
}

// TestPluginNodeTargetsUnknownAgent covers the configuration error: a
// node_targets entry naming an agent the document does not define.
func TestPluginNodeTargetsUnknownAgent(t *testing.T) {
	t.Parallel()
	c := newTestCraft(t, nodeDefinition("missing"), newNodeHost())
	_, err := c.OpenRuntime(context.Background(), DefaultKey, RuntimeOptions{})
	if !errdefs.IsNotFound(err) {
		t.Fatalf("OpenRuntime = %v, want NotFound", err)
	}
	if !strings.Contains(err.Error(), `agent "missing" not found`) {
		t.Fatalf("OpenRuntime error = %v, want it to name the missing agent", err)
	}
}

func mustDefinition(t *testing.T, text string) Definition {
	t.Helper()
	def, err := ParseDefinition([]byte(text))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	return def
}

// TestPluginNodeHandlerWritesBoard covers the plugin-facing call
// contract: the payload the plugin receives and the writes it returns.
func TestPluginNodeHandlerWritesBoard(t *testing.T) {
	t.Parallel()
	host := newNodeHost()
	board, err := runNodeHandler(t, host, map[string]any{"level": 2})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if got := board.Vars()["result"]; got != "hello" {
		t.Fatalf("board result = %#v, want hello", got)
	}
	if !slices.Equal(host.calls, []string{"hello.node_echo"}) {
		t.Fatalf("plugin calls = %v", host.calls)
	}
	payload, ok := host.args[0].(map[string]any)
	if !ok {
		t.Fatalf("plugin payload = %#v, want an object", host.args[0])
	}
	node, ok := payload["node"].(map[string]any)
	if !ok {
		t.Fatalf("payload node = %#v, want an object", payload["node"])
	}
	wantNode := map[string]any{"id": "n1", "type": "hello.echo", "graph": "g1"}
	for key, value := range wantNode {
		if node[key] != value {
			t.Fatalf("payload node[%s] = %#v, want %#v", key, node[key], value)
		}
	}
	config, ok := payload["config"].(map[string]any)
	if !ok || config["level"] != 2 {
		t.Fatalf("payload config = %#v, want the node config", payload["config"])
	}
}

// TestPluginNodeHandlerSurfacesFailures covers what the host does when
// the plugin does not answer with the writes contract.
func TestPluginNodeHandlerSurfacesFailures(t *testing.T) {
	t.Parallel()
	pluginErr := errors.New("plugin exploded")
	cases := []struct {
		name    string
		reply   func(string, string, any) (json.RawMessage, error)
		wantErr string
	}{{
		name: "call failure propagates",
		reply: func(string, string, any) (json.RawMessage, error) {
			return nil, pluginErr
		},
		wantErr: "plugin exploded",
	}, {
		name: "plain text is not the writes contract",
		reply: func(string, string, any) (json.RawMessage, error) {
			return json.RawMessage(`"done"`), nil
		},
		wantErr: "decode plugin result",
	}, {
		name: "empty result writes nothing",
		reply: func(string, string, any) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := newNodeHost()
			host.reply = tc.reply
			board, err := runNodeHandler(t, host, nil)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("handler: %v", err)
				}
				if got := board.Vars()["result"]; got != nil {
					t.Fatalf("board result = %#v, want nothing written", got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("handler error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestPluginNodeFactoryRejectsBadSettings covers the settings
// validation of the synthesized node type.
func TestPluginNodeFactoryRejectsBadSettings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		settings string
	}{{
		name:     "missing tool",
		settings: `{"type":"hello.echo","plugin":"hello"}`,
	}, {
		name:     "unparsable timeout",
		settings: `{"type":"hello.echo","plugin":"hello","tool":"node_echo","timeout":"soon"}`,
	}, {
		name:     "negative timeout",
		settings: `{"type":"hello.echo","plugin":"hello","tool":"node_echo","timeout":"-1s"}`,
	}, {
		name:     "unknown field",
		settings: `{"type":"hello.echo","plugin":"hello","tool":"node_echo","nope":1}`,
	}}
	factory := pluginNodeFactory{host: newNodeHost()}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := factory.New(context.Background(), resource.Input{
				Settings: []byte(tc.settings),
			})
			if !errdefs.IsValidation(err) {
				t.Fatalf("factory error = %v, want Validation", err)
			}
		})
	}
}

// runNodeHandler builds the node type through the real factory and runs
// its handler once.
func runNodeHandler(
	t *testing.T,
	host *nodeHost,
	config map[string]any,
) (*agent.Board, error) {
	t.Helper()
	factory := pluginNodeFactory{host: host}
	value, err := factory.New(context.Background(), resource.Input{
		Settings: []byte(
			`{"type":"hello.echo","plugin":"hello","tool":"node_echo","timeout":"5s"}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registrar, ok := value.(pluginNodeRegistrar)
	if !ok {
		t.Fatalf("factory returned %T", value)
	}
	board := agent.NewBoard()
	err = registrar.node.Handler(
		coregraph.ExecutionContext{
			Context:  context.Background(),
			NodeID:   "n1",
			NodeType: "hello.echo",
			GraphID:  "g1",
		},
		board,
		config,
	)
	return board, err
}
