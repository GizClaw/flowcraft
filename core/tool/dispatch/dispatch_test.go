package dispatch_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/dispatch"
)

// session is the smallest session a Call can carry: an id and a scratch map.
type session struct {
	id      string
	scratch map[string]any
}

func (s *session) ID() string { return s.id }
func (s *session) Scratch() tool.Scratch {
	if s.scratch == nil {
		s.scratch = map[string]any{}
	}
	return mapScratch(s.scratch)
}

type mapScratch map[string]any

func (m mapScratch) Load(key string) (any, bool) { v, ok := m[key]; return v, ok }
func (m mapScratch) Store(key string, value any) { m[key] = value }
func (m mapScratch) Delete(key string)           { delete(m, key) }

// files is a stand-in for a session capability, so the tests can prove one is
// unreachable without declaring whose it is.
type files struct{ content string }

var filesSlot = tool.NewSlot[*files]("test.files")

// pocket is what a host assembles: one filled capability.
func pocket() *tool.CapSet {
	set := tool.NewCapSet()
	filesSlot.Provide(set, &files{content: "content"})
	return set
}

// bound builds the binding a host would: a session, a grant that allows every wired
// tool, and the session's capability pocket.
func bound(t *testing.T) tool.Binding {
	t.Helper()
	return tool.Bind(&session{id: "s1"}, policy.Grant{}).WithCaps(pocket())
}

// registryOf builds a process that provides the capability these tests use: the
// registration gate is exercised on its own in
// TestRegistrationRefusesANeedTheProcessDoesNotProvide.
func registryOf(t *testing.T, tools ...tool.Tool) *tool.Registry {
	t.Helper()
	registry, err := tool.NewRegistry(nil, tool.WithProvisioned(pocket()))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	for _, one := range tools {
		if err := registry.Add(one); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	return registry
}

func request(name string) message.ToolCall {
	return message.ToolCall{ID: "call-1", Name: name, Arguments: message.NewArguments([]byte(`{}`))}
}

func TestRefusesAnUnboundCapability(t *testing.T) {
	table := registryOf(t, tool.TextTool(definition("echo"), func(context.Context, string) (string, error) {
		return "ok", nil
	}))
	executor := dispatch.New(table)

	result := executor.Execute(context.Background(), tool.Binding{Grant: policy.Grant{}}, request("echo"))
	if !result.IsError {
		t.Fatal("an unbound capability was accepted")
	}
	if !contains(result, "unbound") {
		t.Fatalf("result = %q, want it to name the unbound capability", result.Content.Text())
	}
}

func TestRefusesAToolTheGrantDoesNotAllow(t *testing.T) {
	table := registryOf(t, tool.TextTool(definition("echo"), func(context.Context, string) (string, error) {
		return "ok", nil
	}))
	executor := dispatch.New(table)

	call := tool.Binding{
		Grant:   policy.Grant{Tools: policy.ToolSet{Allow: []string{"read_file"}}},
		Session: &session{id: "s1"},
	}
	result := executor.Execute(context.Background(), call, request("echo"))
	if !result.IsError {
		t.Fatal("a tool outside the grant was executed")
	}
	if _, ok := policy.AsDenial(nil); ok {
		t.Fatal("unreachable")
	}
}

func TestRefusesAnUnknownTool(t *testing.T) {
	executor := dispatch.New(registryOf(t))
	result := executor.Execute(context.Background(), bound(t), request("absent"))
	if !result.IsError {
		t.Fatal("an unknown tool was executed")
	}
}

// TestRegistrationRefusesANeedTheProcessDoesNotProvide is the construction-time
// gate: a process that cannot serve a tool says so when the tool is registered, not
// at the first call that reaches for the capability.
func TestRegistrationRefusesANeedTheProcessDoesNotProvide(t *testing.T) {
	reader := tool.CapFunc([]tool.Need{filesSlot}, definition("read_file"),
		func(_ context.Context, b tool.Binding, _ string) (message.Content, error) {
			if _, ok := filesSlot.Get(b.Caps); !ok {
				return message.Content{}, errNoFiles
			}
			return message.NewTextContent("content"), nil
		})

	registry, err := tool.NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := registry.Add(reader); err == nil {
		t.Fatal("a registry that provides no capability accepted a tool that needs one")
	}

	provided, err := tool.NewRegistry(nil, tool.WithProvisioned(pocket()))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := provided.Add(reader); err != nil {
		t.Fatalf("a process that provides the capability refused the tool: %v", err)
	}
}

// TestDeclaredCapabilityIsReadable: the pocket reaches a tool that asked for it.
func TestDeclaredCapabilityIsReadable(t *testing.T) {
	var content string
	reader := tool.CapFunc([]tool.Need{filesSlot}, definition("read_file"),
		func(_ context.Context, b tool.Binding, _ string) (message.Content, error) {
			got, ok := filesSlot.Get(b.Caps)
			if !ok {
				return message.Content{}, errNoFiles
			}
			content = got.content
			return message.NewTextContent(got.content), nil
		})
	executor := dispatch.New(registryOf(t, reader))

	if result := executor.Execute(context.Background(), bound(t), request("read_file")); result.IsError {
		t.Fatalf("Execute: %q", result.Content.Text())
	}
	if content != "content" {
		t.Fatalf("capability = %q, want the session's", content)
	}
}

// TestCapabilityIsNarrowedToWhatTheToolDeclared: a tool that declared nothing
// observes nothing, whatever the host assembled.
func TestCapabilityIsNarrowedToWhatTheToolDeclared(t *testing.T) {
	var saw tool.Binding
	plain := tool.CapFunc(nil, definition("plain"),
		func(_ context.Context, b tool.Binding, _ string) (message.Content, error) {
			saw = b
			return message.NewTextContent("ok"), nil
		})
	executor := dispatch.New(registryOf(t, plain))

	if result := executor.Execute(context.Background(), bound(t), request("plain")); result.IsError {
		t.Fatalf("Execute: %q", result.Content.Text())
	}
	if _, ok := filesSlot.Get(saw.Caps); ok {
		t.Fatal("a tool that declared no need could read the session's capability")
	}
	if saw.Caps.Len() != 0 {
		t.Fatalf("pocket Len = %d, want 0", saw.Caps.Len())
	}
	if saw.Session == nil {
		t.Fatal("the session is not a declared need; it is always present")
	}
}

// TestScratchIsPartitionedByTool pins the collision the partition prevents: two tools
// of one session using the same key must not see each other's value.
func TestScratchIsPartitionedByTool(t *testing.T) {
	session := &session{id: "s1"}
	store := func(name, value string) tool.Tool {
		return tool.TextTool(definition(name), func(_ context.Context, _ string) (string, error) {
			return value, nil
		})
	}
	_ = store

	writer := tool.CapFunc(nil, definition("writer"), func(_ context.Context, call tool.Binding, _ string) (message.Content, error) {
		call.Session.Scratch().Store("cursor", "writer-value")
		return message.NewTextContent("ok"), nil
	})
	reader := tool.CapFunc(nil, definition("reader"), func(_ context.Context, call tool.Binding, _ string) (message.Content, error) {
		value, ok := call.Session.Scratch().Load("cursor")
		if !ok {
			return message.NewTextContent("unset"), nil
		}
		return message.NewTextContent(value.(string)), nil
	})

	registry, err := tool.NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	for _, one := range []tool.Tool{writer, reader} {
		if err := registry.Add(one); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	executor := dispatch.New(registry)
	call := tool.Binding{Grant: policy.Grant{}, Session: session}

	// The same key on the underlying map, but a different namespace per tool.
	call.Session.Scratch().Store("reader.cursor", "sentinel")
	if result := executor.Execute(context.Background(), call, request("writer")); result.IsError {
		t.Fatalf("writer: %q", result.Content.Text())
	}
	result := executor.Execute(context.Background(), call, request("reader"))
	if result.IsError {
		t.Fatalf("reader: %q", result.Content.Text())
	}
	if got := result.Content.Text(); got != "sentinel" {
		t.Fatalf("reader saw %q, want its own value", got)
	}
}

func definition(name string) message.ToolDefinition {
	return message.ToolDefinition{Name: name, InputSchema: []byte(`{"type":"object"}`)}
}

func contains(result message.ToolResult, want string) bool {
	for _, part := range result.Content.Parts {
		if text, ok := part.(message.TextPart); ok && len(text.Text) >= len(want) {
			for i := 0; i+len(want) <= len(text.Text); i++ {
				if text.Text[i:i+len(want)] == want {
					return true
				}
			}
		}
	}
	return false
}

var errNoFiles = errTest("no file boundary")

type errTest string

func (e errTest) Error() string { return string(e) }
