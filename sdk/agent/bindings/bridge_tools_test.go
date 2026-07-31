package bindings

import (
	"context"
	"sort"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/tool"
)

// The tool bridge is a plain map[string]any of closures, so its
// behaviour is exercised by calling the map directly — no script VM
// needed. (The VM-side "maps are injected as globals" contract is
// covered by the runtime packages' own tests; keeping these tests in
// pure Go also avoids an sdk → sdkx test-only module dependency.)

type toolAPI struct {
	call func(name, args string) map[string]any
	list func() []string
}

func newToolAPI(t *testing.T, dispatcher tool.Dispatcher, catalog tool.Catalog, opts ...ToolBridgeOption) toolAPI {
	t.Helper()
	name, raw := NewToolBridge(dispatcher, catalog, opts...)(context.Background())
	if name != "tools" {
		t.Fatalf("binding name = %q, want %q", name, "tools")
	}
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("binding value = %T, want map[string]any", raw)
	}
	call, ok := m["call"].(func(string, string) (map[string]any, error))
	if !ok {
		t.Fatalf("tools.call = %T", m["call"])
	}
	list, ok := m["list"].(func() []string)
	if !ok {
		t.Fatalf("tools.list = %T", m["list"])
	}
	return toolAPI{
		call: func(name, args string) map[string]any {
			res, err := call(name, args)
			if err != nil {
				t.Fatalf("tools.call(%q) returned Go error: %v", name, err)
			}
			return res
		},
		list: list,
	}
}

func newEchoTools(t *testing.T, names ...string) (tool.Dispatcher, tool.Catalog) {
	t.Helper()
	reg := tool.NewRegistry()
	for _, n := range names {
		name := n
		reg.Register(tool.FuncTool(
			tool.Definition{Name: name, Description: name},
			func(_ context.Context, args string) (string, error) {
				return "got:" + name + ":" + args, nil
			},
		))
	}
	return tool.NewExecutor(reg), reg
}

func TestToolBridge_Allowed(t *testing.T) {
	dispatcher, catalog := newEchoTools(t, "echo")
	api := newToolAPI(t, dispatcher, catalog, WithAllowedToolNames("echo"))

	res := api.call("echo", `{"x":1}`)
	if res["is_error"] == true {
		t.Fatalf("call failed: %v", res["content"])
	}
	if got, want := res["content"], `got:echo:{"x":1}`; got != want {
		t.Fatalf("content = %v, want %v", got, want)
	}
	if res["tool_call_id"] == "" {
		t.Fatal("tool_call_id should be populated")
	}

	names := api.list()
	if len(names) != 1 || names[0] != "echo" {
		t.Fatalf("list = %v, want [echo]", names)
	}
}

func TestToolBridge_DenyByDefault(t *testing.T) {
	dispatcher, catalog := newEchoTools(t, "echo")
	api := newToolAPI(t, dispatcher, catalog)

	res := api.call("echo", "{}")
	if res["is_error"] != true {
		t.Fatalf("expected deny, got %v", res)
	}
	if names := api.list(); len(names) != 0 {
		t.Fatalf("list should be empty under default deny, got %v", names)
	}
}

func TestToolBridge_AllowAll(t *testing.T) {
	dispatcher, catalog := newEchoTools(t, "a", "b")
	api := newToolAPI(t, dispatcher, catalog, WithToolAllowAll())

	if res := api.call("a", "{}"); res["is_error"] == true || res["content"] != "got:a:{}" {
		t.Fatalf("a failed: %v", res)
	}
	if res := api.call("b", "{}"); res["is_error"] == true || res["content"] != "got:b:{}" {
		t.Fatalf("b failed: %v", res)
	}

	names := api.list()
	sort.Strings(names)
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("list = %v, want [a b]", names)
	}
}

func TestToolBridge_AllowAll_UnknownTool(t *testing.T) {
	// Even under AllowAll, calling a tool the catalog doesn't know
	// about must surface as is_error (vs. silently invoking nil).
	dispatcher, catalog := newEchoTools(t, "known")
	api := newToolAPI(t, dispatcher, catalog, WithToolAllowAll())

	res := api.call("ghost", "{}")
	if res["is_error"] != true {
		t.Fatalf("expected is_error for unknown tool, got %v", res)
	}
	if content, _ := res["content"].(string); content == "" || !contains(content, "ghost") {
		t.Fatalf("error content should mention the missing tool name: %v", res["content"])
	}
}

func TestToolBridge_NilDispatcher(t *testing.T) {
	// nil dispatcher/catalog must not panic; call() returns is_error
	// and list() returns an empty list. Mirrors the FS bridge's
	// nil-workspace contract.
	api := newToolAPI(t, nil, nil, WithToolAllowAll())

	res := api.call("anything", "{}")
	if res["is_error"] != true {
		t.Fatalf("expected is_error with nil dispatcher, got %v", res)
	}
	if names := api.list(); len(names) != 0 {
		t.Fatalf("list should be empty with nil catalog, got %v", names)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
