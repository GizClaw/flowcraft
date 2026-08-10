package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/config/utils"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
	sdktool "github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdkx/tool/dynamic"
	"github.com/GizClaw/flowcraft/sdkx/tool/mcp"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------------------------------------------------------------------------
// test server harness
// ---------------------------------------------------------------------------

// testServer is an in-process MCP server wired to a client over
// net.Pipe. It exists so the discovery, call, notification, and
// teardown paths are exercised over a real protocol session rather than
// a mocked session object — the reconcile logic depends on go-sdk
// behaviour (cache invalidation ordering, notification delivery) that a
// mock would not reproduce.
type testServer struct {
	server          *mcpsdk.Server
	transport       mcpsdk.Transport
	serverTransport mcpsdk.Transport
	done            chan struct{}
}

func newTestServer(t *testing.T, name string) *testServer {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: name, Version: "v0"}, nil)
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	return &testServer{
		server:          srv,
		transport:       clientT,
		serverTransport: serverT,
		done:            make(chan struct{}),
	}
}

// serve starts the server session. Called after tools are added so the
// initial tools/list sees them.
func (ts *testServer) serve(t *testing.T) {
	t.Helper()
	session, err := ts.server.Connect(context.Background(), ts.serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	go func() {
		defer close(ts.done)
		_ = session.Wait()
	}()
	t.Cleanup(func() { _ = session.Close() })
}

// addTool registers a tool whose handler returns the given content.
func (ts *testServer) addTool(name, description string, schema any, ann *mcpsdk.ToolAnnotations, handler mcpsdk.ToolHandler) {
	ts.server.AddTool(&mcpsdk.Tool{
		Name:        name,
		Description: description,
		InputSchema: schema,
		Annotations: ann,
	}, handler)
}

func (ts *testServer) addResource(uri, name, description, mimeType, text string) {
	ts.server.AddResource(&mcpsdk.Resource{
		URI:         uri,
		Name:        name,
		Description: description,
		MIMEType:    mimeType,
	}, func(_ context.Context, _ *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
		return &mcpsdk.ReadResourceResult{
			Contents: []*mcpsdk.ResourceContents{
				{URI: uri, MIMEType: mimeType, Text: text},
			},
		}, nil
	})
}

func textResult(text string) mcpsdk.ToolHandler {
	return func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
		}, nil
	}
}

func objectSchema() any {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
}

// ---------------------------------------------------------------------------
// discovery + registration
// ---------------------------------------------------------------------------

func TestAddServer_RegistersNamespacedTools(t *testing.T) {
	ts := newTestServer(t, "fs")
	ts.addTool("read_file", "Read a file", objectSchema(), nil, textResult("contents"))
	ts.addTool("write_file", "Write a file", objectSchema(), nil, textResult("written"))
	ts.serve(t)

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })

	if err := src.AddServer(t.Context(), "filesystem", ts.transport); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	for _, want := range []string{"filesystem__read_file", "filesystem__write_file"} {
		if _, ok := reg.Get(want); !ok {
			t.Errorf("registry is missing %q; has %v", want, reg.Names())
		}
	}
	if reg.Len() != 2 {
		t.Errorf("registry Len = %d, want 2 (%v)", reg.Len(), reg.Names())
	}
}

// TestAddServer_CoexistsWithBuiltins is the property the whole design
// rests on: after attaching, a built-in tool and an MCP tool are both
// just entries in one registry, so one Executor dispatches both.
func TestAddServer_CoexistsWithBuiltins(t *testing.T) {
	ts := newTestServer(t, "fs")
	ts.addTool("read_file", "Read a file", objectSchema(), nil, textResult("mcp-content"))
	ts.serve(t)

	reg := sdktool.NewRegistry()
	reg.Register(sdktool.FuncTool(
		message.DefineSchema("builtin_echo", "echo").Build(),
		func(_ context.Context, args string) (string, error) { return "echo:" + args, nil },
	))

	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })
	if err := src.AddServer(t.Context(), "fs", ts.transport); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	executor := sdktool.NewExecutor(reg)
	for _, tc := range []struct{ name, want string }{
		{"builtin_echo", "echo:{}"},
		{"fs__read_file", "mcp-content"},
	} {
		res := executor.Execute(t.Context(), message.Call{
			ID: "c1", Name: tc.name, Arguments: json.RawMessage(`{}`),
		})
		if res.IsError {
			t.Errorf("%s: unexpected error result %q", tc.name, res.Content)
			continue
		}
		if res.Content != tc.want {
			t.Errorf("%s: content = %q, want %q", tc.name, res.Content, tc.want)
		}
	}
}

// TestAddServer_NamespacingAvoidsCollision covers PRD AE2: two servers
// exposing the same tool name both register and stay addressable.
func TestAddServer_NamespacingAvoidsCollision(t *testing.T) {
	first := newTestServer(t, "a")
	first.addTool("search", "Search A", objectSchema(), nil, textResult("from-a"))
	first.serve(t)

	second := newTestServer(t, "b")
	second.addTool("search", "Search B", objectSchema(), nil, textResult("from-b"))
	second.serve(t)

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })

	if err := src.AddServer(t.Context(), "alpha", first.transport); err != nil {
		t.Fatalf("AddServer alpha: %v", err)
	}
	if err := src.AddServer(t.Context(), "beta", second.transport); err != nil {
		t.Fatalf("AddServer beta: %v", err)
	}

	executor := sdktool.NewExecutor(reg)
	for name, want := range map[string]string{
		"alpha__search": "from-a",
		"beta__search":  "from-b",
	} {
		res := executor.Execute(t.Context(), message.Call{
			ID: "c1", Name: name, Arguments: json.RawMessage(`{}`),
		})
		if res.Content != want {
			t.Errorf("%s: content = %q, want %q", name, res.Content, want)
		}
	}
}

func TestAddServer_RejectsExplicitPrefixCollision(t *testing.T) {
	first := newTestServer(t, "a")
	first.addTool("read", "Read A", objectSchema(), nil, textResult("from-a"))
	first.serve(t)

	second := newTestServer(t, "b")
	second.addTool("read", "Read B", objectSchema(), nil, textResult("from-b"))
	second.serve(t)

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })

	if err := src.AddServer(t.Context(), "alpha", first.transport,
		mcp.WithPrefix("shared__")); err != nil {
		t.Fatalf("AddServer alpha: %v", err)
	}
	err := src.AddServer(t.Context(), "beta", second.transport,
		mcp.WithPrefix("shared__"))
	if err == nil || !errdefs.IsConflict(err) {
		t.Fatalf("AddServer beta = %v, want Conflict-classified error", err)
	}

	if _, ok := reg.Get("shared__read"); !ok {
		t.Fatal("first server's tool was lost")
	}
	if got := src.Servers(); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("attached servers = %v, want only alpha", got)
	}
}

func TestAddServer_WithPrefixAndScope(t *testing.T) {
	ts := newTestServer(t, "fs")
	ts.addTool("read_file", "Read", objectSchema(), nil, textResult("x"))
	ts.serve(t)

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })

	err := src.AddServer(t.Context(), "fs", ts.transport,
		mcp.WithPrefix(""), mcp.WithScope(sdktool.ScopePlatform))
	if err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if _, ok := reg.Get("read_file"); !ok {
		t.Fatalf("empty prefix should register the bare name; got %v", reg.Names())
	}
	if got := reg.ScopeOf("read_file"); got != sdktool.ScopePlatform {
		t.Errorf("ScopeOf = %q, want %q", got, sdktool.ScopePlatform)
	}
	if defs := reg.DefinitionsByScope(sdktool.ScopeAgent); len(defs) != 0 {
		t.Errorf("platform-scoped tool leaked into agent scope: %v", defs)
	}
}

func TestAddServer_RejectsBadInput(t *testing.T) {
	src := mcp.NewSource(sdktool.NewRegistry())
	t.Cleanup(func() { _ = src.Close() })

	if err := src.AddServer(t.Context(), "", nil); !errdefs.IsValidation(err) {
		t.Errorf("empty name: want Validation, got %v", err)
	}
	if err := src.AddServer(t.Context(), "fs", nil); !errdefs.IsValidation(err) {
		t.Errorf("nil transport: want Validation, got %v", err)
	}
}

func TestAddServer_RejectsDuplicateName(t *testing.T) {
	ts := newTestServer(t, "fs")
	ts.addTool("read_file", "Read", objectSchema(), nil, textResult("x"))
	ts.serve(t)

	other := newTestServer(t, "fs2")
	other.addTool("read_file", "Read", objectSchema(), nil, textResult("y"))
	other.serve(t)

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })

	if err := src.AddServer(t.Context(), "fs", ts.transport); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	err := src.AddServer(t.Context(), "fs", other.transport)
	if !errdefs.IsValidation(err) {
		t.Errorf("duplicate name: want Validation, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// error mapping
// ---------------------------------------------------------------------------

// TestExecute_LostServerFailsOnlyItsOwnTools covers PRD AE4: losing one
// server degrades only its tools. Detaching is the observable path for
// this — a crashed transport and an explicit detach converge on the same
// state, a server whose session is gone — and it is the one a test can
// drive deterministically, since tearing down the in-memory server side
// blocks while the client still holds a request open.
func TestExecute_LostServerFailsOnlyItsOwnTools(t *testing.T) {
	lost := newTestServer(t, "lost")
	lost.addTool("read", "Read", objectSchema(), nil, textResult("from-lost"))
	lost.serve(t)

	healthy := newTestServer(t, "healthy")
	healthy.addTool("read", "Read", objectSchema(), nil, textResult("from-healthy"))
	healthy.serve(t)

	reg := sdktool.NewRegistry()
	reg.Register(sdktool.FuncTool(
		message.DefineSchema("builtin_echo", "echo").Build(),
		func(_ context.Context, args string) (string, error) { return "echo", nil },
	))

	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })
	if err := src.AddServer(t.Context(), "lost", lost.transport); err != nil {
		t.Fatalf("AddServer lost: %v", err)
	}
	if err := src.AddServer(t.Context(), "healthy", healthy.transport); err != nil {
		t.Fatalf("AddServer healthy: %v", err)
	}

	// Grab the adapted tool before detaching: a caller holding a tool
	// value across the loss is the case that must fail cleanly rather
	// than reach a closed session.
	orphaned, ok := reg.Get("lost__read")
	if !ok {
		t.Fatal("lost__read not registered")
	}
	if err := src.RemoveServer("lost"); err != nil {
		t.Fatalf("RemoveServer: %v", err)
	}

	if _, err := orphaned.Execute(t.Context(), `{}`); !errdefs.IsNotAvailable(err) {
		t.Errorf("orphaned tool: want NotAvailable, got %v", err)
	} else if !strings.Contains(err.Error(), "lost") {
		t.Errorf("orphaned tool error = %v, want it to name the server", err)
	}

	executor := sdktool.NewExecutor(reg)
	call := func(name string) message.Result {
		return executor.Execute(t.Context(), message.Call{
			ID: "c1", Name: name, Arguments: json.RawMessage(`{}`),
		})
	}
	if got := call("healthy__read"); got.IsError || got.Content != "from-healthy" {
		t.Errorf("healthy server: got %+v, want content %q", got, "from-healthy")
	}
	if got := call("builtin_echo"); got.IsError || got.Content != "echo" {
		t.Errorf("built-in tool: got %+v, want content %q", got, "echo")
	}
}

// TestAddServer_UnreachableServerIsNotAvailable pins the connect-failure
// classification: a command that does not exist is an environment
// problem, so it must be NotAvailable and must leave the registry clean
// for the host to carry on with its other servers.
func TestAddServer_UnreachableServerIsNotAvailable(t *testing.T) {
	transport, err := mcp.Stdio("flowcraft-nonexistent-mcp-server", nil, nil)
	if err != nil {
		t.Fatalf("Stdio: %v", err)
	}

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })

	err = src.AddServer(t.Context(), "missing", transport)
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("missing command: want NotAvailable, got %v", err)
	}
	if reg.Len() != 0 {
		t.Errorf("failed attach left %d tools registered: %v", reg.Len(), reg.Names())
	}
	if len(src.Servers()) != 0 {
		t.Errorf("failed attach left the server attached: %v", src.Servers())
	}
}

// ---------------------------------------------------------------------------
// result rendering
// ---------------------------------------------------------------------------

func TestResult_RendersTextContent(t *testing.T) {
	ts := newTestServer(t, "fs")
	ts.addTool("echo", "Echo", objectSchema(), nil, func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "hello world"}},
		}, nil
	})
	ts.serve(t)

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })
	if err := src.AddServer(t.Context(), "fs", ts.transport); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	executor := sdktool.NewExecutor(reg)
	res := executor.Execute(t.Context(), message.Call{
		ID: "c1", Name: "fs__echo", Arguments: json.RawMessage(`{}`),
	})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	if res.Content != "hello world" {
		t.Errorf("content = %q, want %q", res.Content, "hello world")
	}
}

func TestResult_IsErrorReturnsNonNilError(t *testing.T) {
	ts := newTestServer(t, "fs")
	ts.addTool("fail", "Fail", objectSchema(), nil, func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "permission denied"}},
			IsError: true,
		}, nil
	})
	ts.serve(t)

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })
	if err := src.AddServer(t.Context(), "fs", ts.transport); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	executor := sdktool.NewExecutor(reg)
	res := executor.Execute(t.Context(), message.Call{
		ID: "c1", Name: "fs__fail", Arguments: json.RawMessage(`{}`),
	})
	if !res.IsError {
		t.Fatalf("expected isError, got content %q", res.Content)
	}
	if !strings.Contains(res.Content, "permission denied") {
		t.Errorf("error = %q, want something containing 'permission denied'", res.Content)
	}
}

func TestResult_MultiPartContent(t *testing.T) {
	ts := newTestServer(t, "fs")
	ts.addTool("describe", "Describe", objectSchema(), nil, func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{
				&mcpsdk.TextContent{Text: "name: foo"},
				&mcpsdk.TextContent{Text: "size: 42"},
			},
		}, nil
	})
	ts.serve(t)

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })
	if err := src.AddServer(t.Context(), "fs", ts.transport); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	executor := sdktool.NewExecutor(reg)
	res := executor.Execute(t.Context(), message.Call{
		ID: "c1", Name: "fs__describe", Arguments: json.RawMessage(`{}`),
	})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	want := "name: foo\nsize: 42"
	if res.Content != want {
		t.Errorf("content = %q, want %q", res.Content, want)
	}
}

// ---------------------------------------------------------------------------
// schema
// ---------------------------------------------------------------------------

// TestSchema_RegisteredDefinitionsValidate asserts the invariant every
// consumer depends on: whatever a server sends, the Definition handed to
// the registry is a valid one, so a malformed remote schema can never
// poison an inference request.
func TestSchema_RegisteredDefinitionsValidate(t *testing.T) {
	ts := newTestServer(t, "fs")
	ts.addTool("typed", "Typed", objectSchema(), nil, textResult("x"))
	ts.addTool("bare", "Bare", json.RawMessage(`{"type":"object"}`), nil, textResult("y"))
	ts.serve(t)

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })
	if err := src.AddServer(t.Context(), "fs", ts.transport); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	defs := reg.Definitions()
	if len(defs) != 2 {
		t.Fatalf("len(Definitions) = %d, want 2", len(defs))
	}
	for _, def := range defs {
		if err := def.Validate(); err != nil {
			t.Errorf("definition %q does not validate: %v", def.Name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// metadata
// ---------------------------------------------------------------------------

func TestMetadata_ReadOnlyHintMapsToMutatesState(t *testing.T) {
	ts := newTestServer(t, "fs")
	ts.addTool("read", "Read", objectSchema(), &mcpsdk.ToolAnnotations{ReadOnlyHint: true}, textResult("x"))
	ts.addTool("write", "Write", objectSchema(), &mcpsdk.ToolAnnotations{ReadOnlyHint: false}, textResult("y"))
	ts.addTool("default", "Default", objectSchema(), nil, textResult("z"))
	ts.serve(t)

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })
	if err := src.AddServer(t.Context(), "fs", ts.transport); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	for _, tc := range []struct {
		name        string
		wantMutates bool
	}{
		{"fs__read", false},
		{"fs__write", true},
		{"fs__default", true},
	} {
		tool, ok := reg.Get(tc.name)
		if !ok {
			t.Fatalf("tool %q not registered", tc.name)
		}
		meta := sdktool.MetadataOf(tool)
		if meta.MutatesState != tc.wantMutates {
			t.Errorf("%s: MutatesState = %v, want %v", tc.name, meta.MutatesState, tc.wantMutates)
		}
		if !meta.SelfTimeout {
			t.Errorf("%s: SelfTimeout should be true for MCP tools", tc.name)
		}
	}
}

// ---------------------------------------------------------------------------
// notification-driven refresh
// ---------------------------------------------------------------------------

func TestRefresh_RegistryReconcilesOnToolListChanged(t *testing.T) {
	ts := newTestServer(t, "fs")
	ts.addTool("first", "First", objectSchema(), nil, textResult("from-first"))
	ts.serve(t)

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })
	if err := src.AddServer(t.Context(), "fs", ts.transport); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if reg.Len() != 1 {
		t.Fatalf("after AddServer: Len = %d, want 1", reg.Len())
	}

	// Add a second tool on the server side.
	ts.addTool("second", "Second", objectSchema(), nil, textResult("from-second"))

	// Refresh triggers re-list + reconcile.
	if err := src.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if reg.Len() != 2 {
		t.Fatalf("after Refresh: Len = %d, want 2; names: %v", reg.Len(), reg.Names())
	}
	if _, ok := reg.Get("fs__second"); !ok {
		t.Fatalf("fs__second not registered after Refresh; names: %v", reg.Names())
	}
}

func TestRefresh_RemovesStaleTools(t *testing.T) {
	// Use RemoveTools which sends list_changed notification.
	ts := newTestServer(t, "fs")
	ts.addTool("keep", "Keep", objectSchema(), nil, textResult("kept"))
	ts.addTool("remove", "Remove", objectSchema(), nil, textResult("gone"))
	ts.serve(t)

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })
	if err := src.AddServer(t.Context(), "fs", ts.transport); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if reg.Len() != 2 {
		t.Fatalf("after AddServer: Len = %d, want 2", reg.Len())
	}

	// Remove a tool on the server side.
	ts.server.RemoveTools("remove")

	if err := src.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if reg.Len() != 1 {
		t.Fatalf("after Refresh: Len = %d, want 1; names: %v", reg.Len(), reg.Names())
	}
	if _, ok := reg.Get("fs__remove"); ok {
		t.Errorf("stale tool fs__remove still registered")
	}
	if _, ok := reg.Get("fs__keep"); !ok {
		t.Errorf("kept tool fs__keep missing after Refresh")
	}
}

// ---------------------------------------------------------------------------
// RemoveServer
// ---------------------------------------------------------------------------

func TestRemoveServer_UnregistersTools(t *testing.T) {
	ts := newTestServer(t, "fs")
	ts.addTool("read", "Read", objectSchema(), nil, textResult("x"))
	ts.serve(t)

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })
	if err := src.AddServer(t.Context(), "fs", ts.transport); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if reg.Len() != 1 {
		t.Fatalf("after AddServer: Len = %d, want 1", reg.Len())
	}

	if err := src.RemoveServer("fs"); err != nil {
		t.Fatalf("RemoveServer: %v", err)
	}
	if reg.Len() != 0 {
		t.Errorf("after RemoveServer: Len = %d, want 0", reg.Len())
	}
}

// ---------------------------------------------------------------------------
// Close
// ---------------------------------------------------------------------------

func TestClose_Idempotent(t *testing.T) {
	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	if err := src.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// config bridge
// ---------------------------------------------------------------------------

// specNode parses a spec body into the opaque settings subtree a config
// document would hand a source factory.
func specNode(t *testing.T, body string) json.RawMessage {
	t.Helper()
	jsonData, err := utils.ToJSON([]byte(body))
	if err != nil {
		t.Fatalf("specNode(%q): %v", body, err)
	}
	var out json.RawMessage
	if err := json.Unmarshal(jsonData, &out); err != nil {
		t.Fatalf("specNode(%q): %v", body, err)
	}
	return out
}

// TestConfig_ParseSpecAcceptsBothTransports pins the declarative shape
// hosts write in YAML. Attaching is covered by the AddServer tests; what
// matters here is that a valid document decodes and an invalid one is
// rejected at parse time rather than at first call.
func TestConfig_ParseSpecAcceptsBothTransports(t *testing.T) {
	spec, err := mcp.ParseSpec(specNode(t, `
servers:
  - name: files
    transport: stdio
    command: npx
    args: ["-y", "server"]
    env: {TOKEN: x}
  - name: remote
    transport: http
    url: https://mcp.example.com
    headers: {Authorization: "Bearer x"}
    scope: platform
    prefix: ""
`))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if len(spec.Servers) != 2 {
		t.Fatalf("len(Servers) = %d, want 2", len(spec.Servers))
	}
	stdio := spec.Servers[0]
	if stdio.Command != "npx" || len(stdio.Args) != 2 || stdio.Env["TOKEN"] != "x" {
		t.Errorf("stdio server decoded as %+v", stdio)
	}
	remote := spec.Servers[1]
	if remote.URL != "https://mcp.example.com" || remote.Scope != sdktool.ScopePlatform {
		t.Errorf("http server decoded as %+v", remote)
	}
	if remote.Prefix == nil || *remote.Prefix != "" {
		t.Errorf("explicit empty prefix should decode to a non-nil empty string, got %v", remote.Prefix)
	}
}

// TestConfig_SourceFactoryAttaches drives the factory end to end: decode
// a spec, attach it to a registry, and confirm the tools land there and
// leave again on Close. The stdio command is a real MCP server run as a
// subprocess, which is also what proves the transport wiring works
// outside the in-memory harness.
func TestConfig_SourceFactoryRejectsBadSpec(t *testing.T) {
	if _, err := mcp.SourceFactory(t.Context(), sdkconfig.Input{
		Settings: specNode(t, `servers: []`),
	}); err == nil {
		t.Error("SourceFactory with no servers succeeded, want error")
	}
	if _, err := mcp.SourceFactory(t.Context(), sdkconfig.Input{}); err == nil {
		t.Error("SourceFactory with nil spec succeeded, want error")
	}
}

func TestConfig_ParseSpecRejectsBadInput(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"empty", `{}`},
		{"servers not a list", `{"servers": 3}`},
		{"bad transport", `{"servers":[{"name":"x","transport":"telnet"}]}`},
		{"missing name", `{"servers":[{"transport":"stdio","command":"x"}]}`},
		{"duplicate name", `{"servers":[{"name":"a","transport":"stdio","command":"x"},{"name":"a","transport":"stdio","command":"y"}]}`},
		{"stdio with url", `{"servers":[{"name":"x","transport":"stdio","command":"x","url":"http://x"}]}`},
		{"http with command", `{"servers":[{"name":"x","transport":"http","url":"http://x","command":"y"}]}`},
		{"bad scope", `{"servers":[{"name":"x","transport":"stdio","command":"x","scope":"secret"}]}`},
		{"bad exposure", `{"servers":[{"name":"x","transport":"stdio","command":"x","exposure":"secret"}]}`},
		{"bad tool exposure", `{"servers":[{"name":"x","transport":"stdio","command":"x","tools":{"read_file":"secret"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := mcp.ParseSpec(specNode(t, tc.raw)); err == nil {
				t.Errorf("ParseSpec(%q) succeeded, want error", tc.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// deferred attach + exposure
// ---------------------------------------------------------------------------

func TestAddServer_DeferredRegistersDeclaredProxyWithoutConnecting(t *testing.T) {
	ts := newTestServer(t, "fs")
	ts.addTool("read_file", "Read a file", objectSchema(), nil, textResult("contents"))

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })

	err := src.AddServer(t.Context(), "filesystem", ts.transport,
		mcp.WithDeferred(true),
		mcp.WithTools(map[string]dynamic.Exposure{"read_file": dynamic.ExposureDeferred}),
	)
	if err != nil {
		t.Fatalf("AddServer(deferred): %v", err)
	}

	tool, ok := reg.Get("filesystem__read_file")
	if !ok {
		t.Fatalf("declared proxy missing from registry; have %v", reg.Names())
	}
	lazy, isLazy := tool.(*dynamic.LazyTool)
	if !isLazy {
		t.Fatalf("declared tool type = %T, want *dynamic.LazyTool", tool)
	}
	if def := lazy.Definition(); def.Name != "filesystem__read_file" ||
		!strings.Contains(def.Description, "deferred") {
		t.Errorf("placeholder definition = %+v", def)
	}
	if got := src.ToolNames("filesystem"); len(got) != 1 || got[0] != "filesystem__read_file" {
		t.Errorf("ToolNames = %v, want the declared proxy", got)
	}
	if reg.Len() != 1 {
		t.Errorf("registry Len = %d, want only the declared proxy", reg.Len())
	}

	// Serve the server side, then load; the proxy is replaced by the
	// real adapted tool and stays callable.
	ts.serve(t)
	if err := src.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh(deferred): %v", err)
	}
	tool, ok = reg.Get("filesystem__read_file")
	if !ok {
		t.Fatal("tool missing after load")
	}
	if _, isLazy := tool.(*dynamic.LazyTool); isLazy {
		t.Fatal("tool is still a proxy after load, want adapted tool")
	}
	res, err := tool.Execute(t.Context(), `{}`)
	if err != nil {
		t.Fatalf("Execute after load: %v", err)
	}
	if res != "contents" {
		t.Errorf("Execute = %q, want contents", res)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestAddServer_DeferredExecutesThroughProxy(t *testing.T) {
	ts := newTestServer(t, "fs")
	ts.addTool("read_file", "Read a file", objectSchema(), nil, textResult("mcp-content"))
	ts.serve(t)

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })

	if err := src.AddServer(t.Context(), "filesystem", ts.transport,
		mcp.WithDeferred(true),
		mcp.WithTools(map[string]dynamic.Exposure{"read_file": dynamic.ExposureDeferred}),
	); err != nil {
		t.Fatalf("AddServer(deferred): %v", err)
	}

	tool, ok := reg.Get("filesystem__read_file")
	if !ok {
		t.Fatal("declared proxy missing")
	}
	res, err := tool.Execute(t.Context(), `{}`)
	if err != nil {
		t.Fatalf("Execute through proxy: %v", err)
	}
	if res != "mcp-content" {
		t.Errorf("Execute = %q, want mcp-content", res)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestAddServer_DeferredExposureWithDynamicCatalog(t *testing.T) {
	ts := newTestServer(t, "fs")
	ts.addTool("read_file", "Read a file", objectSchema(), nil, textResult("x"))
	ts.addTool("write_file", "Write a file", objectSchema(), nil, textResult("x"))

	reg := sdktool.NewRegistry()
	dyn := dynamic.New(reg)
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })
	t.Cleanup(func() { _ = dyn.Close() })

	err := src.AddServer(t.Context(), "filesystem", ts.transport,
		mcp.WithDeferred(true),
		mcp.WithDynamicCatalog(dyn),
		mcp.WithExposure(dynamic.ExposureDeferred),
		mcp.WithTools(map[string]dynamic.Exposure{
			"read_file": dynamic.ExposureAlways,
		}),
	)
	if err != nil {
		t.Fatalf("AddServer(deferred): %v", err)
	}

	// read_file is always-visible even as a placeholder; write_file
	// stays deferred and hidden until selected.
	defs := dyn.Definitions()
	if len(defs) != 1 || defs[0].Name != "filesystem__read_file" {
		t.Fatalf("dynamic Definitions = %v, want only read_file placeholder", defs)
	}

	ts.serve(t)
	if err := src.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	defs = dyn.Definitions()
	if len(defs) != 1 || defs[0].Name != "filesystem__read_file" {
		t.Fatalf("dynamic Definitions after load = %v", defs)
	}
	if defs[0].Description != "Read a file" {
		t.Errorf("loaded description = %q, want real description", defs[0].Description)
	}

	dyn.Select("filesystem__write_file")
	got := make([]string, 0, 2)
	for _, d := range dyn.Definitions() {
		got = append(got, d.Name)
	}
	want := []string{"filesystem__read_file", "filesystem__write_file"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Definitions after select = %v, want %v", got, want)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestAddServer_DeferredRejectsInvalidExposure(t *testing.T) {
	src := mcp.NewSource(sdktool.NewRegistry())
	t.Cleanup(func() { _ = src.Close() })
	err := src.AddServer(t.Context(), "fs", &mcpsdk.CommandTransport{},
		mcp.WithDeferred(true),
		mcp.WithTools(map[string]dynamic.Exposure{"x": dynamic.Exposure("bogus")}),
	)
	if !errdefs.IsValidation(err) {
		t.Fatalf("AddServer = %v, want Validation", err)
	}
}

func TestAddServer_DeferredOwnershipConflict(t *testing.T) {
	first := newTestServer(t, "a")
	first.addTool("read", "Read A", objectSchema(), nil, textResult("a"))
	second := newTestServer(t, "b")
	second.addTool("read", "Read B", objectSchema(), nil, textResult("b"))

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })

	tools := map[string]dynamic.Exposure{"read": dynamic.ExposureDeferred}
	if err := src.AddServer(t.Context(), "alpha", first.transport,
		mcp.WithDeferred(true), mcp.WithPrefix("shared__"), mcp.WithTools(tools)); err != nil {
		t.Fatalf("AddServer alpha: %v", err)
	}
	err := src.AddServer(t.Context(), "beta", second.transport,
		mcp.WithDeferred(true), mcp.WithPrefix("shared__"), mcp.WithTools(tools))
	if err == nil || !errdefs.IsConflict(err) {
		t.Fatalf("AddServer beta = %v, want Conflict", err)
	}
	if got := src.Servers(); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("attached servers = %v, want only alpha", got)
	}
}

func TestApplyExposures_SyncServer(t *testing.T) {
	ts := newTestServer(t, "fs")
	ts.addTool("read_file", "Read a file", objectSchema(), nil, textResult("x"))
	ts.addTool("write_file", "Write a file", objectSchema(), nil, textResult("x"))
	ts.serve(t)

	reg := sdktool.NewRegistry()
	dyn := dynamic.New(reg)
	t.Cleanup(func() { _ = dyn.Close() })
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })

	if err := src.AddServer(t.Context(), "filesystem", ts.transport,
		mcp.WithExposure(dynamic.ExposureAlways),
		mcp.WithTools(map[string]dynamic.Exposure{"read_file": dynamic.ExposureHidden}),
	); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if err := src.ApplyExposures(dyn); err != nil {
		t.Fatalf("ApplyExposures: %v", err)
	}
	got := make([]string, 0, 1)
	for _, d := range dyn.Definitions() {
		got = append(got, d.Name)
	}
	if len(got) != 1 || got[0] != "filesystem__write_file" {
		t.Errorf("Definitions = %v, want only write_file (read_file hidden)", got)
	}
}

// ---------------------------------------------------------------------------
// resources bridge
// ---------------------------------------------------------------------------

func TestAddServer_ResourcesBridge(t *testing.T) {
	ts := newTestServer(t, "fs")
	ts.addTool("read_file", "Read a file", objectSchema(), nil, textResult("x"))
	ts.addResource("file:///tmp/a.txt", "a.txt", "A text file", "text/plain", "hello")
	ts.serve(t)

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })

	if err := src.AddServer(t.Context(), "filesystem", ts.transport,
		mcp.WithResources(true)); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	for _, name := range []string{
		"filesystem__list_resources",
		"filesystem__read_resource",
	} {
		if _, ok := reg.Get(name); !ok {
			t.Fatalf("resource bridge tool %q missing; have %v", name, reg.Names())
		}
	}

	list, _ := reg.Get("filesystem__list_resources")
	raw, err := list.Execute(t.Context(), `{}`)
	if err != nil {
		t.Fatalf("list resources: %v", err)
	}
	if !strings.Contains(raw, "file:///tmp/a.txt") ||
		!strings.Contains(raw, "A text file") {
		t.Errorf("list output = %s, want resource metadata", raw)
	}

	read, _ := reg.Get("filesystem__read_resource")
	raw, err = read.Execute(t.Context(), `{"uri":"file:///tmp/a.txt"}`)
	if err != nil {
		t.Fatalf("read resource: %v", err)
	}
	if !strings.Contains(raw, "hello") {
		t.Errorf("read output = %s, want resource text", raw)
	}

	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestAddServer_DeferredResourcesBridgeLoadsOnRefresh(t *testing.T) {
	ts := newTestServer(t, "fs")
	ts.addResource("file:///tmp/a.txt", "a.txt", "A text file", "text/plain", "hello")
	ts.serve(t)

	reg := sdktool.NewRegistry()
	src := mcp.NewSource(reg)
	t.Cleanup(func() { _ = src.Close() })

	if err := src.AddServer(t.Context(), "filesystem", ts.transport,
		mcp.WithDeferred(true), mcp.WithResources(true)); err != nil {
		t.Fatalf("AddServer(deferred): %v", err)
	}
	if _, ok := reg.Get("filesystem__list_resources"); ok {
		t.Fatal("resource bridge tools must not register before the deferred load")
	}
	if err := src.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	read, ok := reg.Get("filesystem__read_resource")
	if !ok {
		t.Fatalf("resource bridge missing after load; have %v", reg.Names())
	}
	raw, err := read.Execute(t.Context(), `{"uri":"file:///tmp/a.txt"}`)
	if err != nil {
		t.Fatalf("read resource: %v", err)
	}
	if !strings.Contains(raw, "hello") {
		t.Errorf("read output = %s, want resource text", raw)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestConfig_ParseSpecAcceptsResources(t *testing.T) {
	spec, err := mcp.ParseSpec(specNode(t, `{
		"servers": [{
			"name": "x",
			"transport": "stdio",
			"command": "x",
			"resources": true
		}]
	}`))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if len(spec.Servers) != 1 || !spec.Servers[0].Resources {
		t.Errorf("parsed spec = %+v, want resources enabled", spec.Servers)
	}
}
