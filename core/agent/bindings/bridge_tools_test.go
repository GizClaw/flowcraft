package bindings

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// The tool bridge is a plain map[string]any of closures, so its
// behaviour is exercised by calling the map directly — no script VM
// needed. (The VM-side "maps are injected as globals" contract is
// covered by the runtime packages' own tests; keeping these tests in
// pure Go also avoids an sdk → integrations test-only module dependency.)

type toolAPI struct {
	call        func(name, args string) map[string]any
	callAll     func(items []any) []map[string]any
	list        func() []string
	definitions func() []any
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
	callAll, ok := m["callAll"].(func(any) ([]map[string]any, error))
	if !ok {
		t.Fatalf("tools.callAll = %T", m["callAll"])
	}
	list, ok := m["list"].(func() []string)
	if !ok {
		t.Fatalf("tools.list = %T", m["list"])
	}
	definitions, ok := m["definitions"].(func() ([]any, error))
	if !ok {
		t.Fatalf("tools.definitions = %T", m["definitions"])
	}
	return toolAPI{
		call: func(name, args string) map[string]any {
			res, err := call(name, args)
			if err != nil {
				t.Fatalf("tools.call(%q) returned Go error: %v", name, err)
			}
			return res
		},
		callAll: func(items []any) []map[string]any {
			res, err := callAll(items)
			if err != nil {
				t.Fatalf("tools.callAll returned Go error: %v", err)
			}
			return res
		},
		list: list,
		definitions: func() []any {
			defs, err := definitions()
			if err != nil {
				t.Fatalf("tools.definitions returned Go error: %v", err)
			}
			return defs
		},
	}
}

type toolSource struct {
	tools []tool.Tool
}

func (s toolSource) Tools() []tool.Tool         { return s.tools }
func (s toolSource) LazyTools() []tool.LazyTool { return nil }

func newEchoTools(t *testing.T, names ...string) (tool.Dispatcher, tool.Catalog) {
	t.Helper()
	var tools []tool.Tool
	for _, n := range names {
		name := n
		tools = append(tools, tool.FuncTool(
			message.ToolDefinition{Name: name, Description: name},
			func(_ context.Context, args string) (string, error) {
				return "got:" + name + ":" + args, nil
			},
		))
	}
	reg, err := tool.NewRegistry([]tool.Source{toolSource{tools: tools}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
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

func TestToolBridge_CallAll_OrderPreserved(t *testing.T) {
	dispatcher, catalog := newEchoTools(t, "echo")
	api := newToolAPI(t, dispatcher, catalog, WithAllowedToolNames("echo"))

	out := api.callAll([]any{
		map[string]any{"name": "echo", "arguments": `{"n":1}`},
		map[string]any{"name": "echo", "arguments": `{"n":2}`},
		map[string]any{"name": "echo", "arguments": `{"n":3}`},
	})
	if len(out) != 3 {
		t.Fatalf("callAll returned %d results, want 3", len(out))
	}
	for i, want := range []string{`got:echo:{"n":1}`, `got:echo:{"n":2}`, `got:echo:{"n":3}`} {
		if out[i]["content"] != want || out[i]["is_error"] == true {
			t.Fatalf("result[%d] = %v, want content %q", i, out[i], want)
		}
		if out[i]["name"] != "echo" {
			t.Fatalf("result[%d].name = %v, want echo", i, out[i]["name"])
		}
	}
}

func TestToolBridge_CallAll_ForwardsModelIssuedID(t *testing.T) {
	// A script continuing an LLM turn must echo the model-issued call
	// id back in the tool_result — providers match results by id.
	dispatcher, catalog := newEchoTools(t, "echo")
	api := newToolAPI(t, dispatcher, catalog, WithAllowedToolNames("echo"))

	out := api.callAll([]any{
		map[string]any{"id": "call_abc123", "name": "echo", "arguments": `{}`},
	})
	if out[0]["tool_call_id"] != "call_abc123" {
		t.Fatalf("tool_call_id = %v, want call_abc123 forwarded verbatim", out[0]["tool_call_id"])
	}
}

func TestToolBridge_CallAll_MintsIDWhenAbsent(t *testing.T) {
	dispatcher, catalog := newEchoTools(t, "echo")
	api := newToolAPI(t, dispatcher, catalog, WithAllowedToolNames("echo"))

	out := api.callAll([]any{map[string]any{"name": "echo", "arguments": `{}`}})
	if out[0]["tool_call_id"] == "" {
		t.Fatal("expected a minted tool_call_id when the entry omits id")
	}
}

func TestToolBridge_CallAll_PerEntryDeny(t *testing.T) {
	dispatcher, catalog := newEchoTools(t, "echo", "rm")
	api := newToolAPI(t, dispatcher, catalog, WithAllowedToolNames("echo"))

	out := api.callAll([]any{
		map[string]any{"name": "echo", "arguments": `{"n":1}`},
		map[string]any{"name": "rm", "arguments": `{}`},
		map[string]any{"name": "echo", "arguments": `{"n":2}`},
	})
	if len(out) != 3 {
		t.Fatalf("callAll returned %d results, want 3", len(out))
	}
	if out[0]["is_error"] == true || out[2]["is_error"] == true {
		t.Fatalf("allowed entries should succeed: %v", out)
	}
	if out[1]["is_error"] != true {
		t.Fatalf("denied entry should be is_error in place: %v", out[1])
	}
	content, _ := out[1]["content"].(string)
	if !strings.Contains(content, "not allowed") || out[1]["name"] != "rm" {
		t.Fatalf("denied entry = %v, want not-allowed error named rm", out[1])
	}
}

func TestToolBridge_CallAll_AllowAllUnknownTool(t *testing.T) {
	dispatcher, catalog := newEchoTools(t, "known")
	api := newToolAPI(t, dispatcher, catalog, WithToolAllowAll())

	out := api.callAll([]any{map[string]any{"name": "ghost", "arguments": `{}`}})
	if out[0]["is_error"] != true {
		t.Fatalf("unknown tool under allowAll = %v, want is_error", out[0])
	}
}

func TestToolBridge_CallAll_NoDispatcher(t *testing.T) {
	api := newToolAPI(t, nil, nil, WithToolAllowAll())
	out := api.callAll([]any{
		map[string]any{"name": "echo", "arguments": `{}`},
		map[string]any{"name": "echo", "arguments": `{}`},
	})
	for i, res := range out {
		if res["is_error"] != true {
			t.Fatalf("result[%d] = %v, want is_error without dispatcher", i, res)
		}
	}
}

func TestToolBridge_CallAll_EmptyBatch(t *testing.T) {
	dispatcher, catalog := newEchoTools(t, "echo")
	api := newToolAPI(t, dispatcher, catalog, WithToolAllowAll())
	if out := api.callAll(nil); len(out) != 0 {
		t.Fatalf("empty batch = %v, want empty", out)
	}
}

func TestToolBridge_CallAll_ArgumentsDefault(t *testing.T) {
	dispatcher, catalog := newEchoTools(t, "echo")
	api := newToolAPI(t, dispatcher, catalog, WithAllowedToolNames("echo"))

	out := api.callAll([]any{map[string]any{"name": "echo"}})
	if out[0]["content"] != "got:echo:{}" {
		t.Fatalf("omitted arguments should default to {}: %v", out[0])
	}
}

func TestToolBridge_Definitions(t *testing.T) {
	dispatcher, catalog := newEchoTools(t, "echo", "rm")
	api := newToolAPI(t, dispatcher, catalog, WithAllowedToolNames("echo"))

	defs := api.definitions()
	if len(defs) != 1 {
		t.Fatalf("definitions() = %v, want the allowed subset only", defs)
	}
	def, ok := defs[0].(map[string]any)
	if !ok || def["name"] != "echo" || def["description"] != "echo" {
		t.Fatalf("definition = %v, want wire JSON with name/description", defs[0])
	}

	full := newToolAPI(t, dispatcher, catalog, WithToolAllowAll())
	if got := full.definitions(); len(got) != 2 {
		t.Fatalf("allowAll definitions() = %d entries, want 2", len(got))
	}

	none := newToolAPI(t, nil, nil, WithToolAllowAll())
	if got := none.definitions(); len(got) != 0 {
		t.Fatalf("nil catalog definitions() = %v, want empty", got)
	}
}

func TestToolBridge_CallAll_Validation(t *testing.T) {
	dispatcher, catalog := newEchoTools(t, "echo")
	_, raw := NewToolBridge(dispatcher, catalog, WithToolAllowAll())(context.Background())
	callAll := raw.(map[string]any)["callAll"].(func(any) ([]map[string]any, error))

	cases := []struct {
		name  string
		items []any
	}{
		{"non object entry", []any{"echo"}},
		{"missing name", []any{map[string]any{"arguments": `{}`}}},
		{"non string id", []any{map[string]any{"name": "echo", "id": 42}}},
		{"non string arguments", []any{map[string]any{"name": "echo", "arguments": map[string]any{"q": "x"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := callAll(tc.items)
			if err == nil {
				t.Fatalf("callAll(%v) should fail validation", tc.items)
			}
			if !errdefs.IsValidation(err) {
				t.Fatalf("callAll(%v) error = %v, want validation-classified", tc.items, err)
			}
		})
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
