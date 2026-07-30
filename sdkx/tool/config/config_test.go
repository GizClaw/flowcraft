package config_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/tool/middleware"
	"github.com/GizClaw/flowcraft/sdkx/tool/config"
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

	exec, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// scope metadata applied
	if got := registry.ScopeOf("exec"); got != tool.ScopePlatform {
		t.Errorf("ScopeOf(exec) = %q, want platform", got)
	}

	// approval gates exec: denied, tool never runs, audit still records
	res := exec.Execute(context.Background(), call("exec"))
	if !res.IsError {
		t.Fatal("gated call should be denied by approver")
	}
	if sink.records != 1 {
		t.Errorf("audit records = %d, want 1 (denial observed by outer audit)", sink.records)
	}

	// ungated tool passes the whole chain
	res = exec.Execute(context.Background(), call("echo"))
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
		return func(_ context.Context, _ json.RawMessage) (tool.Middleware, error) {
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

	exec, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	exec.Execute(context.Background(), call("x"))
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
				{Kind: "nope"}}},
			tools:   []string{"x"},
			wantErr: "unknown kind",
		},
		{
			name: "scope on unregistered tool",
			doc: config.Document{Version: "v1", Scopes: map[string]string{
				"ghost": tool.ScopePlatform}},
			tools:   []string{"x"},
			wantErr: "not registered",
		},
		{
			name: "approval without approver dep",
			doc: config.Document{Version: "v1", Middlewares: []config.MiddlewareEntry{
				{Kind: "approval", Spec: json.RawMessage(`{"tools":["x"]}`)}}},
			tools:   []string{"x"},
			wantErr: "Approver",
		},
		{
			name: "audit without sink dep",
			doc: config.Document{Version: "v1", Middlewares: []config.MiddlewareEntry{
				{Kind: "audit"}}},
			tools:   []string{"x"},
			wantErr: "AuditSink",
		},
		{
			name: "concurrency zero limit",
			doc: config.Document{Version: "v1", Middlewares: []config.MiddlewareEntry{
				{Kind: "concurrency", Spec: json.RawMessage(`{"limit":0}`)}}},
			tools:   []string{"x"},
			wantErr: "limit",
		},
		{
			name: "concurrency unknown spec field",
			doc: config.Document{Version: "v1", Middlewares: []config.MiddlewareEntry{
				{Kind: "concurrency", Spec: json.RawMessage(`{"limits":10}`)}}},
			tools:   []string{"x"},
			wantErr: "unknown field",
		},
		{
			name: "approval empty tools",
			doc: config.Document{Version: "v1", Middlewares: []config.MiddlewareEntry{
				{Kind: "approval", Spec: json.RawMessage(`{"tools":[]}`)}}},
			deps: config.Deps{Approver: middleware.ApproverFunc(
				func(context.Context, tool.Call) error { return nil })},
			tools:   []string{"x"},
			wantErr: "at least one",
		},
		{
			name: "recover rejects spec",
			doc: config.Document{Version: "v1", Middlewares: []config.MiddlewareEntry{
				{Kind: "recover", Spec: json.RawMessage(`{"x":1}`)}}},
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
	exec, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	start := time.Now()
	res := exec.Execute(context.Background(), call("hang"))
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
		{Kind: "timeout", Spec: json.RawMessage(`{"default":30}`)}}}
	if _, err := builder.Build(context.Background(), doc); err == nil {
		t.Error("numeric duration should be rejected; units belong in the file")
	}
}
