package config_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/tool/middleware"
	"github.com/GizClaw/flowcraft/sdkx/tool/config"
	yamlv3 "gopkg.in/yaml.v3"
)

func echoTool(name string) tool.Tool {
	return tool.FuncTool(tool.Definition{Name: name},
		func(_ context.Context, args string) (string, error) {
			return "echo:" + args, nil
		})
}

func call(name string) tool.Call {
	return tool.Call{ID: "c1", Name: name, Arguments: json.RawMessage(`{"x":1}`)}
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

func TestParse_StrictRejects(t *testing.T) {
	cases := map[string]string{
		"unknown top-level field": "version: v1\nbogus: 1\n",
		"bad version":             "version: v2\n",
		"empty kind":              "version: v1\nmiddlewares:\n  - spec: {}\n",
		"bad scope":               "version: v1\nscopes: { exec: secret }\n",
		"multiple documents":      "version: v1\n---\nversion: v1\n",
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

	registry := tool.NewRegistry()
	registry.Register(echoTool("exec"))
	registry.Register(echoTool("echo"))

	sink := &spySink{}
	approver := middleware.ApproverFunc(func(_ context.Context, _ tool.Call) error {
		return errors.New("not today")
	})
	builder := config.NewBuilder(registry, config.Deps{Approver: approver, AuditSink: sink})

	assembly, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = assembly.Close() })

	// scope metadata applied
	if got := registry.ScopeOf("exec"); got != tool.ScopePlatform {
		t.Errorf("ScopeOf(exec) = %q, want platform", got)
	}

	// approval gates exec: denied, tool never runs, audit still records
	res := assembly.Executor.Execute(context.Background(), call("exec"))
	if !res.IsError {
		t.Fatal("gated call should be denied by approver")
	}
	if sink.records != 1 {
		t.Errorf("audit records = %d, want 1 (denial observed by outer audit)", sink.records)
	}

	// ungated tool passes the whole chain
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
		Middlewares: []config.MiddlewareEntry{
			{Kind: "first"}, {Kind: "second"},
		},
	}
	registry := tool.NewRegistry()
	registry.Register(echoTool("x"))

	var order []string
	track := func(label string) config.MiddlewareFactory {
		return func(_ context.Context, _ *yamlv3.Node) (tool.Middleware, error) {
			return func(next tool.Dispatch) tool.Dispatch {
				return func(ctx context.Context, c tool.Call) tool.Result {
					order = append(order, label)
					return next(ctx, c)
				}
			}, nil
		}
	}
	builder := config.NewBuilder(registry, config.Deps{})
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
	registryWith := func(names ...string) *tool.Registry {
		r := tool.NewRegistry()
		for _, n := range names {
			r.Register(echoTool(n))
		}
		return r
	}
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
			tools:   []string{"x"},
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
				{Kind: "approval", Spec: yamlSpec(t, `tools: [x]`)},
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
				{Kind: "concurrency", Spec: yamlSpec(t, `limit: 0`)},
			}},
			tools:   []string{"x"},
			wantErr: "limit",
		},
		{
			name: "concurrency unknown spec field",
			doc: config.Document{Version: "v1", Middlewares: []config.MiddlewareEntry{
				{Kind: "concurrency", Spec: yamlSpec(t, `limits: 10`)},
			}},
			tools:   []string{"x"},
			wantErr: "field limits not found",
		},
		{
			name: "approval empty tools",
			doc: config.Document{Version: "v1", Middlewares: []config.MiddlewareEntry{
				{Kind: "approval", Spec: yamlSpec(t, `tools: []`)},
			}},
			deps: config.Deps{Approver: middleware.ApproverFunc(
				func(context.Context, tool.Call) error { return nil },
			)},
			tools:   []string{"x"},
			wantErr: "at least one",
		},
		{
			name: "recover rejects spec",
			doc: config.Document{Version: "v1", Middlewares: []config.MiddlewareEntry{
				{Kind: "recover", Spec: yamlSpec(t, `x: 1`)},
			}},
			tools:   []string{"x"},
			wantErr: "takes no spec",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			builder := config.NewBuilder(registryWith(tc.tools...), tc.deps)
			_, err := builder.Build(context.Background(), tc.doc)
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
// Timeout spec decoding
// ---------------------------------------------------------------------------

func TestTimeoutSpec_DurationStrings(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Register(tool.FuncTool(tool.Definition{Name: "hang"},
		func(ctx context.Context, _ string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}))
	builder := config.NewBuilder(registry, config.Deps{})

	doc, err := config.Parse([]byte(`
version: v1
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
	builder := config.NewBuilder(tool.NewRegistry(), config.Deps{})
	doc := config.Document{Version: "v1", Middlewares: []config.MiddlewareEntry{
		{Kind: "timeout", Spec: yamlSpec(t, `default: 30`)},
	}}
	if _, err := builder.Build(context.Background(), doc); err == nil {
		t.Error("numeric duration should be rejected; units belong in the file")
	}
}

// ---------------------------------------------------------------------------
// sources
// ---------------------------------------------------------------------------

// fakeSource is a Source that registers a fixed set of tools, so the
// Builder's ordering and cleanup guarantees can be tested without an
// external process.
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
			Spec: yamlSpec(t, `{}`),
		})
	}
	return doc
}

// yamlSpec builds an opaque spec subtree the way Parse would, so a test
// constructing a Document by hand exercises the same factory decode
// path a real YAML file does.
func yamlSpec(t *testing.T, body string) *config.Opaque {
	t.Helper()
	var doc struct {
		Spec *config.Opaque `yaml:"spec"`
	}
	if err := yamlv3.Unmarshal([]byte("spec:\n  "+strings.ReplaceAll(body, "\n", "\n  ")), &doc); err != nil {
		t.Fatalf("yamlSpec(%q): %v", body, err)
	}
	return doc.Spec
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
		Name      string `yaml:"name"`
		Transport string `yaml:"transport"`
		Command   string `yaml:"command"`
	}
	spec, err := config.DecodeSpec[struct {
		Servers []serverSpec `yaml:"servers"`
	}](doc.Sources[0].Spec.Node())
	if err != nil {
		t.Fatalf("source spec is not decodable YAML: %v", err)
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

// TestBuild_SourceToolsJoinTheRegistry is the whole point of the sources
// layer: a tool a source provides is dispatchable through the same
// Executor as a hand-registered one, with the same middleware applied.
func TestBuild_SourceToolsJoinTheRegistry(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Register(echoTool("builtin"))

	builder := config.NewBuilder(registry, config.Deps{})
	source := &fakeSource{tools: []string{"remote_a", "remote_b"}}
	builder.RegisterSourceFactory("fake", func(context.Context, *yamlv3.Node) (config.Source, error) {
		return source, nil
	})

	assembly, err := builder.Build(context.Background(), sourceDoc(t, "fake"))
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
	if _, ok := registry.Get("remote_a"); ok {
		t.Error("Close did not unregister the source's tools")
	}
	if _, ok := registry.Get("builtin"); !ok {
		t.Error("Close removed a tool the source did not own")
	}
}

// TestBuild_SourceAttachesBeforeScopes proves the ordering that makes a
// scope entry able to name a source-provided tool.
func TestBuild_SourceAttachesBeforeScopes(t *testing.T) {
	registry := tool.NewRegistry()
	builder := config.NewBuilder(registry, config.Deps{})
	builder.RegisterSourceFactory("fake", func(context.Context, *yamlv3.Node) (config.Source, error) {
		return &fakeSource{tools: []string{"remote"}}, nil
	})

	doc := sourceDoc(t, "fake")
	doc.Scopes = map[string]string{"remote": tool.ScopePlatform}

	assembly, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = assembly.Close() })

	if got := registry.ScopeOf("remote"); got != tool.ScopePlatform {
		t.Errorf("ScopeOf(remote) = %q, want %q", got, tool.ScopePlatform)
	}
}

func TestBuild_UnknownSourceKind(t *testing.T) {
	builder := config.NewBuilder(tool.NewRegistry(), config.Deps{})
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

// TestBuild_AttachFailureClosesEarlierSources is the guarantee that keeps
// a bad document from leaking child processes: the first failure unwinds
// everything already attached.
func TestBuild_AttachFailureClosesEarlierSources(t *testing.T) {
	registry := tool.NewRegistry()
	builder := config.NewBuilder(registry, config.Deps{})

	good := &fakeSource{tools: []string{"remote_ok"}}
	bad := &fakeSource{attachErr: errors.New("connect refused")}
	builder.RegisterSourceFactory("good", func(context.Context, *yamlv3.Node) (config.Source, error) {
		return good, nil
	})
	builder.RegisterSourceFactory("bad", func(context.Context, *yamlv3.Node) (config.Source, error) {
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
	if registry.Len() != 0 {
		t.Errorf("failed build left %d tools registered: %v", registry.Len(), registry.Names())
	}
}

// TestBuild_MiddlewareFailureClosesSources covers the same unwinding for a
// failure that happens after every source attached.
func TestBuild_MiddlewareFailureClosesSources(t *testing.T) {
	registry := tool.NewRegistry()
	builder := config.NewBuilder(registry, config.Deps{})
	source := &fakeSource{tools: []string{"remote"}}
	builder.RegisterSourceFactory("fake", func(context.Context, *yamlv3.Node) (config.Source, error) {
		return source, nil
	})

	doc := sourceDoc(t, "fake")
	// approval without an Approver in Deps fails at factory time.
	doc.Middlewares = []config.MiddlewareEntry{{
		Kind: config.KindApproval,
		Spec: yamlSpec(t, `tools: [remote]`),
	}}

	if _, err := builder.Build(context.Background(), doc); err == nil {
		t.Fatal("Build succeeded without an Approver, want error")
	}
	if source.closed != 1 {
		t.Errorf("source closed = %d, want 1", source.closed)
	}
	if registry.Len() != 0 {
		t.Errorf("failed build left %d tools registered", registry.Len())
	}
}

func TestBuild_NilSourceFromFactory(t *testing.T) {
	builder := config.NewBuilder(tool.NewRegistry(), config.Deps{})
	builder.RegisterSourceFactory("nil", func(context.Context, *yamlv3.Node) (config.Source, error) {
		return nil, nil
	})
	if _, err := builder.Build(context.Background(), sourceDoc(t, "nil")); err == nil {
		t.Error("Build with a nil source succeeded, want error")
	}
}

func TestAssembly_CloseIsIdempotent(t *testing.T) {
	builder := config.NewBuilder(tool.NewRegistry(), config.Deps{})
	source := &fakeSource{tools: []string{"remote"}}
	builder.RegisterSourceFactory("fake", func(context.Context, *yamlv3.Node) (config.Source, error) {
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
	builder := config.NewBuilder(tool.NewRegistry(), config.Deps{})
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
