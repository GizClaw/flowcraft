package config_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/tool/config"
	"github.com/GizClaw/flowcraft/sdk/tool/middleware"
)

func echoTool(name string) tool.Tool {
	return tool.FuncTool(message.Definition{Name: name},
		func(_ context.Context, args string) (string, error) {
			return "echo:" + args, nil
		})
}

func call(name string) message.Call {
	return message.Call{ID: "c1", Name: name, Arguments: json.RawMessage(`{"x":1}`)}
}

// ---------------------------------------------------------------------------
// Parse
// ---------------------------------------------------------------------------

func TestParse_GoldenYAML(t *testing.T) {
	data, err := os.ReadFile("testdata/executor.yaml")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	doc, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.Version != config.VersionV1 {
		t.Errorf("Version = %q, want v1", doc.Version)
	}
	if len(doc.Sources) != 1 || doc.Sources[0].Kind != config.BuiltinKind {
		t.Fatalf("Sources = %+v, want one builtin entry", doc.Sources)
	}
	if len(doc.Middlewares) != 7 {
		t.Fatalf("len(Middlewares) = %d, want 7", len(doc.Middlewares))
	}
	wantKinds := []string{
		config.KindRecover, config.KindTelemetry, config.KindAudit,
		config.KindConcurrency, config.KindTimeout, config.KindRateLimit,
		config.KindApproval,
	}
	for i, want := range wantKinds {
		if doc.Middlewares[i].Kind != want {
			t.Errorf("Middlewares[%d].Kind = %q, want %q", i, doc.Middlewares[i].Kind, want)
		}
	}
	if doc.Scopes["exec"] != tool.ScopePlatform {
		t.Errorf("Scopes[exec] = %q, want platform", doc.Scopes["exec"])
	}
}

func TestParseJSON(t *testing.T) {
	doc, err := config.Parse([]byte(`{
		"version": "v1",
		"sources": [
			{"kind": "builtin", "spec": {"tools": ["search"]}}
		],
		"middlewares": [
			{"kind": "recover"},
			{"kind": "timeout", "spec": {"default": "30s"}}
		],
		"scopes": {"exec": "platform"}
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(doc.Sources) != 1 || doc.Sources[0].Kind != config.BuiltinKind {
		t.Fatalf("parsed sources = %+v", doc.Sources)
	}
	if len(doc.Middlewares) != 2 || doc.Middlewares[1].Spec == nil {
		t.Fatalf("parsed document = %+v", doc)
	}
}

func TestParse_StrictRejects(t *testing.T) {
	cases := map[string]string{
		"unknown top-level field": "version: v1\nbogus: 1\n",
		"bad version":             "version: v2\n",
		"empty kind":              "version: v1\nmiddlewares:\n  - spec: {}\n",
		"bad scope":               "version: v1\nscopes: { exec: secret }\n",
		"multiple documents":      "version: v1\n---\nversion: v1\n",
		"unknown json field":      `{"version":"v1","bogus":1}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := config.Parse([]byte(input)); err == nil {
				t.Errorf("Parse(%q) succeeded, want error", input)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Build: happy path
// ---------------------------------------------------------------------------

type spySink struct{ records int }

func (s *spySink) Record(_ context.Context, _ middleware.AuditRecord) { s.records++ }

func TestBuild_GoldenChainExecutes(t *testing.T) {
	data, err := os.ReadFile("testdata/executor.yaml")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	doc, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	sink := &spySink{}
	approver := middleware.ApproverFunc(func(_ context.Context, _ message.Call) error {
		return errors.New("not today")
	})
	builder := config.NewBuilder(config.Deps{Approver: approver, AuditSink: sink})
	builder.RegisterBuiltins(echoTool("exec"), echoTool("echo"))

	assembly, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = assembly.Close() })

	registry := assembly.Catalog.(*tool.Registry)
	if got := registry.ScopeOf("exec"); got != tool.ScopePlatform {
		t.Errorf("ScopeOf(exec) = %q, want platform", got)
	}

	res := assembly.Executor.Execute(context.Background(), call("exec"))
	if !res.IsError {
		t.Fatal("gated call should be denied by approver")
	}
	if sink.records != 1 {
		t.Errorf("audit records = %d, want 1 (denial observed by outer audit)", sink.records)
	}

	res = assembly.Executor.Execute(context.Background(), call("echo"))
	if res.IsError {
		t.Fatalf("ungated call failed: %s", res.Content)
	}
	if !strings.HasPrefix(res.Content, "echo:") {
		t.Errorf("Content = %q, want echo: prefix", res.Content)
	}
	if sink.records != 2 {
		t.Errorf("audit records = %d, want 2", sink.records)
	}
}

func TestBuild_ChainOrderIsDocumentOrder(t *testing.T) {
	doc := config.Document{
		Version: config.VersionV1,
		Sources: []config.SourceEntry{builtinSource(t, "x")},
		Middlewares: []config.MiddlewareEntry{
			{Kind: "first"}, {Kind: "second"},
		},
	}

	var order []string
	track := func(label string) config.MiddlewareFactory {
		return func(_ context.Context, _ sdkconfig.Input) (tool.Middleware, error) {
			return func(next tool.Dispatch) tool.Dispatch {
				return func(ctx context.Context, c message.Call) message.Result {
					order = append(order, label)
					return next(ctx, c)
				}
			}, nil
		}
	}
	builder := config.NewBuilder(config.Deps{})
	builder.RegisterBuiltin(echoTool("x"))
	builder.RegisterFactory("first", track("first"))
	builder.RegisterFactory("second", track("second"))

	assembly, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = assembly.Close() })
	assembly.Executor.Execute(context.Background(), call("x"))
	if strings.Join(order, ",") != "first,second" {
		t.Errorf("order = %v, want [first second] (document order = outermost first)", order)
	}
}

// ---------------------------------------------------------------------------
// Build: failure modes
// ---------------------------------------------------------------------------

func TestBuild_FailFast(t *testing.T) {
	cases := []struct {
		name    string
		doc     config.Document
		deps    config.Deps
		tools   []string
		wantErr string
	}{
		{
			name: "unknown kind",
			doc: config.Document{Version: "v1", Middlewares: []config.MiddlewareEntry{
				{Kind: "nope"},
			}},
			wantErr: "unknown kind",
		},
		{
			name: "scope on unregistered tool",
			doc: config.Document{Version: "v1", Scopes: map[string]string{
				"ghost": tool.ScopePlatform,
			}},
			tools:   []string{"x"},
			wantErr: "not registered",
		},
		{
			name: "approval without approver dep",
			doc: config.Document{Version: "v1", Middlewares: []config.MiddlewareEntry{
				{Kind: "approval", Spec: specJSON(t, `{"tools":["x"]}`)},
			}},
			tools:   []string{"x"},
			wantErr: "Approver",
		},
		{
			name: "audit without sink dep",
			doc: config.Document{Version: "v1", Middlewares: []config.MiddlewareEntry{
				{Kind: "audit"},
			}},
			tools:   []string{"x"},
			wantErr: "AuditSink",
		},
		{
			name: "concurrency zero limit",
			doc: config.Document{Version: "v1", Middlewares: []config.MiddlewareEntry{
				{Kind: "concurrency", Spec: specJSON(t, `{"limit":0}`)},
			}},
			tools:   []string{"x"},
			wantErr: "limit",
		},
		{
			name: "concurrency unknown spec field",
			doc: config.Document{Version: "v1", Middlewares: []config.MiddlewareEntry{
				{Kind: "concurrency", Spec: specJSON(t, `{"limits":10}`)},
			}},
			tools:   []string{"x"},
			wantErr: "limits",
		},
		{
			name: "approval empty tools",
			doc: config.Document{Version: "v1", Middlewares: []config.MiddlewareEntry{
				{Kind: "approval", Spec: specJSON(t, `{"tools":[]}`)},
			}},
			deps: config.Deps{Approver: middleware.ApproverFunc(
				func(context.Context, message.Call) error { return nil },
			)},
			tools:   []string{"x"},
			wantErr: "at least one",
		},
		{
			name: "recover rejects spec",
			doc: config.Document{Version: "v1", Middlewares: []config.MiddlewareEntry{
				{Kind: "recover", Spec: specJSON(t, `{"x":1}`)},
			}},
			tools:   []string{"x"},
			wantErr: "takes no spec",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			builder := config.NewBuilder(tc.deps)
			for _, name := range tc.tools {
				builder.RegisterBuiltin(echoTool(name))
			}
			doc := tc.doc
			if len(tc.tools) > 0 {
				doc.Sources = append(
					[]config.SourceEntry{builtinSource(t, tc.tools...)},
					doc.Sources...,
				)
			}
			_, err := builder.Build(context.Background(), doc)
			if err == nil {
				t.Fatal("Build succeeded, want error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want to contain %q", err, tc.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Builtin source
// ---------------------------------------------------------------------------

func TestBuild_BuiltinSourceAttachesTools(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})
	builder.RegisterBuiltins(echoTool("search"), echoTool("exec"))
	doc := config.Document{
		Version: config.VersionV1,
		Sources: []config.SourceEntry{builtinSource(t, "search", "exec")},
	}
	assembly, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = assembly.Close() })
	if _, ok := assembly.Catalog.Get("search"); !ok {
		t.Fatal("builtin tool search missing from catalog")
	}
	if _, ok := assembly.Catalog.Get("exec"); !ok {
		t.Fatal("builtin tool exec missing from catalog")
	}
}

func TestBuild_BuiltinSourceRejectsEmptySpec(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})
	if _, err := builder.Build(context.Background(), sourceDoc(t, config.BuiltinKind)); err == nil {
		t.Fatal("builtin source with empty tools accepted, want error")
	}
}

func TestBuild_UnknownBuiltinTool(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})
	builder.RegisterBuiltin(echoTool("known"))
	doc := config.Document{
		Version: config.VersionV1,
		Sources: []config.SourceEntry{builtinSource(t, "ghost")},
	}
	_, err := builder.Build(context.Background(), doc)
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Build error = %v, want Validation", err)
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error = %v, want to name the missing tool", err)
	}
}

func TestRegisterBuiltin_RejectsBadInput(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})
	assertPanics(t, "nil tool", func() { builder.RegisterBuiltin(nil) })
	builder.RegisterBuiltin(echoTool("x"))
	assertPanics(t, "duplicate tool", func() { builder.RegisterBuiltin(echoTool("x")) })
}

// ---------------------------------------------------------------------------
// Timeout spec decoding
// ---------------------------------------------------------------------------

func TestTimeoutSpec_DurationStrings(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})
	builder.RegisterBuiltin(tool.FuncTool(message.Definition{Name: "hang"},
		func(ctx context.Context, _ string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}))

	doc, err := config.Parse([]byte(`
version: v1
sources:
  - kind: builtin
    spec: {tools: [hang]}
middlewares:
  - kind: timeout
    spec: { default: 50ms, per_tool: { other: 120s } }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	assembly, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = assembly.Close() })
	start := time.Now()
	res := assembly.Executor.Execute(context.Background(), call("hang"))
	if !res.IsError || !strings.Contains(res.Content, "timed out") {
		t.Errorf("expected timeout result, got %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("timeout took %v, per_tool override should not apply to 'hang'", elapsed)
	}
}

func TestTimeoutSpec_RejectsNumericDuration(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})
	doc := config.Document{Version: "v1", Middlewares: []config.MiddlewareEntry{
		{Kind: "timeout", Spec: specJSON(t, `{"default":30}`)},
	}}
	if _, err := builder.Build(context.Background(), doc); err == nil {
		t.Error("numeric duration should be rejected; units belong in the file")
	}
}

// ---------------------------------------------------------------------------
// sources
// ---------------------------------------------------------------------------

type fakeSource struct {
	tools     []string
	attachErr error
	closeErr  error
	registry  *tool.Registry
	attached  int
	closed    int
}

func (f *fakeSource) Attach(_ context.Context, registry *tool.Registry) error {
	f.attached++
	if f.attachErr != nil {
		return f.attachErr
	}
	f.registry = registry
	for _, name := range f.tools {
		registry.Register(echoTool(name))
	}
	return nil
}

func (f *fakeSource) Close() error {
	f.closed++
	if f.registry != nil {
		for _, name := range f.tools {
			f.registry.Unregister(name)
		}
		f.registry = nil
	}
	return f.closeErr
}

func sourceDoc(t *testing.T, kinds ...string) config.Document {
	t.Helper()
	doc := config.Document{Version: config.VersionV1}
	for _, kind := range kinds {
		doc.Sources = append(doc.Sources, config.SourceEntry{
			Kind: kind,
			Spec: specJSON(t, `{}`),
		})
	}
	return doc
}

func builtinSource(t *testing.T, names ...string) config.SourceEntry {
	t.Helper()
	raw, err := json.Marshal(struct {
		Tools []string `json:"tools"`
	}{Tools: names})
	if err != nil {
		t.Fatalf("marshal builtin spec: %v", err)
	}
	return config.SourceEntry{Kind: config.BuiltinKind, Spec: specJSON(t, string(raw))}
}

// specJSON builds an opaque spec subtree the way Parse would, so a test
// constructing a Document by hand exercises the same factory decode
// path a real document does.
func specJSON(t *testing.T, raw string) json.RawMessage {
	t.Helper()
	var out json.RawMessage
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("specJSON(%q): %v", raw, err)
	}
	return out
}

func TestParse_Sources(t *testing.T) {
	doc, err := config.Parse([]byte(`version: v1
sources:
  - kind: mcp
    spec:
      servers:
        - name: files
          transport: stdio
          command: npx
middlewares:
  - kind: recover
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(doc.Sources) != 1 {
		t.Fatalf("len(Sources) = %d, want 1", len(doc.Sources))
	}
	if doc.Sources[0].Kind != "mcp" {
		t.Errorf("Sources[0].Kind = %q, want %q", doc.Sources[0].Kind, "mcp")
	}
	type serverSpec struct {
		Name      string `json:"name"`
		Transport string `json:"transport"`
		Command   string `json:"command"`
	}
	spec, err := config.DecodeSpec[struct {
		Servers []serverSpec `json:"servers"`
	}](doc.Sources[0].Spec)
	if err != nil {
		t.Fatalf("source spec is not decodable: %v", err)
	}
	if len(spec.Servers) != 1 || spec.Servers[0].Name != "files" {
		t.Errorf("source spec decoded as %+v", spec)
	}
}

func TestParse_SourceRequiresKind(t *testing.T) {
	if _, err := config.Parse([]byte("version: v1\nsources:\n  - spec: {}\n")); err == nil {
		t.Error("source without a kind parsed, want error")
	}
}

func TestBuild_SourceToolsJoinTheRegistry(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})
	builder.RegisterBuiltin(echoTool("builtin"))
	source := &fakeSource{tools: []string{"remote_a", "remote_b"}}
	builder.RegisterSourceFactory("fake", func(context.Context, sdkconfig.Input) (config.Source, error) {
		return source, nil
	})

	doc := sourceDoc(t, "fake")
	doc.Sources = append([]config.SourceEntry{builtinSource(t, "builtin")}, doc.Sources...)

	assembly, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if source.attached != 1 {
		t.Errorf("attached = %d, want 1", source.attached)
	}
	for _, name := range []string{"builtin", "remote_a", "remote_b"} {
		res := assembly.Executor.Execute(context.Background(), call(name))
		if res.IsError {
			t.Errorf("%s: unexpected error %q", name, res.Content)
		}
	}

	if err := assembly.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if source.closed != 1 {
		t.Errorf("closed = %d, want 1", source.closed)
	}
	if _, ok := assembly.Catalog.Get("remote_a"); ok {
		t.Error("Close did not unregister the source's tools")
	}
	if _, ok := assembly.Catalog.Get("builtin"); !ok {
		t.Error("Close removed a tool the source did not own")
	}
}

func TestBuild_SourceAttachesBeforeScopes(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})
	builder.RegisterSourceFactory("fake", func(context.Context, sdkconfig.Input) (config.Source, error) {
		return &fakeSource{tools: []string{"remote"}}, nil
	})

	doc := sourceDoc(t, "fake")
	doc.Scopes = map[string]string{"remote": tool.ScopePlatform}

	assembly, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = assembly.Close() })

	registry := assembly.Catalog.(*tool.Registry)
	if got := registry.ScopeOf("remote"); got != tool.ScopePlatform {
		t.Errorf("ScopeOf(remote) = %q, want %q", got, tool.ScopePlatform)
	}
}

func TestBuild_UnknownSourceKind(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})
	_, err := builder.Build(context.Background(), sourceDoc(t, "nope"))
	if err == nil {
		t.Fatal("Build with an unregistered source kind succeeded, want error")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("want Validation, got %v", err)
	}
	if !strings.Contains(err.Error(), "RegisterSourceFactory") {
		t.Errorf("error should point at the fix, got %v", err)
	}
}

func TestBuild_AttachFailureClosesEarlierSources(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})

	good := &fakeSource{tools: []string{"remote_ok"}}
	bad := &fakeSource{attachErr: errors.New("connect refused")}
	builder.RegisterSourceFactory("good", func(context.Context, sdkconfig.Input) (config.Source, error) {
		return good, nil
	})
	builder.RegisterSourceFactory("bad", func(context.Context, sdkconfig.Input) (config.Source, error) {
		return bad, nil
	})

	_, err := builder.Build(context.Background(), sourceDoc(t, "good", "bad"))
	if err == nil {
		t.Fatal("Build succeeded despite a failing source, want error")
	}
	if good.closed != 1 {
		t.Errorf("earlier source closed = %d, want 1", good.closed)
	}
	if bad.closed != 1 {
		t.Errorf("failing source closed = %d, want 1", bad.closed)
	}
	if good.registry != nil || bad.registry != nil {
		t.Error("closed sources still reference a registry")
	}
}

func TestBuild_MiddlewareFailureClosesSources(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})
	source := &fakeSource{tools: []string{"remote"}}
	builder.RegisterSourceFactory("fake", func(context.Context, sdkconfig.Input) (config.Source, error) {
		return source, nil
	})

	doc := sourceDoc(t, "fake")
	doc.Middlewares = []config.MiddlewareEntry{{
		Kind: config.KindApproval,
		Spec: specJSON(t, `{"tools":["remote"]}`),
	}}

	if _, err := builder.Build(context.Background(), doc); err == nil {
		t.Fatal("Build succeeded without an Approver, want error")
	}
	if source.closed != 1 {
		t.Errorf("source closed = %d, want 1", source.closed)
	}
	if source.registry != nil {
		t.Error("closed source still references a registry")
	}
}

func TestBuild_NilSourceFromFactory(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})
	builder.RegisterSourceFactory("nil", func(context.Context, sdkconfig.Input) (config.Source, error) {
		return nil, nil
	})
	if _, err := builder.Build(context.Background(), sourceDoc(t, "nil")); err == nil {
		t.Error("Build with a nil source succeeded, want error")
	}
}

func TestAssembly_CloseIsIdempotent(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})
	source := &fakeSource{tools: []string{"remote"}}
	builder.RegisterSourceFactory("fake", func(context.Context, sdkconfig.Input) (config.Source, error) {
		return source, nil
	})
	assembly, err := builder.Build(context.Background(), sourceDoc(t, "fake"))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := assembly.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := assembly.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if source.closed != 1 {
		t.Errorf("closed = %d, want 1 (Close must not re-close)", source.closed)
	}
}

func TestRegisterSourceFactory_RejectsBadInput(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})
	assertPanics(t, "empty kind", func() { builder.RegisterSourceFactory("", nil) })
	assertPanics(t, "nil factory", func() { builder.RegisterSourceFactory("x", nil) })
}

func assertPanics(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: expected a panic", what)
		}
	}()
	fn()
}
