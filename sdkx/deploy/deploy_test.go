package deploy_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	yamlv3 "gopkg.in/yaml.v3"
)

// ---------- fakes ----------

type fakeEngineFactory struct {
	spec     agent.EngineSpec
	gotCfg   agent.Config
	newCalls int
}

func (f *fakeEngineFactory) Spec() agent.EngineSpec { return f.spec }

func (f *fakeEngineFactory) New(_ context.Context, cfg agent.Config) (agent.Engine, error) {
	f.newCalls++
	f.gotCfg = cfg
	return agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		b.AppendChannelMessage(agent.MainChannel, inference.NewTextMessage(inference.RoleAssistant, "ok"))
		return b, nil
	}), nil
}

// fakeRegistry is a container resource: Lookup resolves named items,
// matching how a workspace / sandbox registry serves refs.
// It records construction order and closure through a shared journal.
type fakeRegistry struct {
	name    string
	items   map[string]string
	journal *journal
}

func (r *fakeRegistry) Lookup(ref string) (any, bool) {
	v, ok := r.items[ref]
	return v, ok
}

func (r *fakeRegistry) Close() error {
	r.journal.closed = append(r.journal.closed, r.name)
	return nil
}

// journal records the order resources were built and closed in, which
// is what the topological-order and reverse-close tests assert on.
type journal struct {
	built  []string
	closed []string
}

// fakeJSRuntime is a whole-bound, non-container, closable resource.
type fakeJSRuntime struct {
	pool    int
	journal *journal
}

func (f *fakeJSRuntime) Close() error {
	f.journal.closed = append(f.journal.closed, "js_main")
	return nil
}

// fakeStore is bound only by hooks, never by an agent dep.
type fakeStore struct{ workspace string }

type recordingHook struct {
	agent.BaseObserver
	store *fakeStore
}

type recordingBefore struct {
	window int
	store  *fakeStore
}

func (r recordingBefore) Before(_ context.Context, _ agent.Identity, req *agent.Request, prev *agent.Board) (*agent.Board, error) {
	b := prev.Clone()
	b.AppendChannelMessage(agent.MainChannel, req.Message)
	b.SetVar("window", r.window)
	return b, nil
}

func graphSpec() agent.EngineSpec {
	return agent.EngineSpec{
		Kind:         "graph",
		Capabilities: agent.Capabilities{SupportsResume: true},
		Deps: []agent.DepSpec{
			{Name: "workspace", Type: "workspace.Workspace", Required: true},
			{Name: "runner", Type: "sandbox.Runner"},
			{Name: "script_runtime", Type: "script.runtime"},
			{Name: "tools", Type: "tool.Catalog"},
		},
	}
}

type testBuilder struct {
	*deploy.Builder
	graph   *fakeEngineFactory
	inline  *fakeEngineFactory
	journal *journal
	hooks   *hookCapture
}

// hookCapture records what the hook factories actually received, so
// tests can assert that resource deps reach the hook layer.
type hookCapture struct {
	before *recordingBefore
	hook   *recordingHook
}

func newTestBuilder(t *testing.T) *testBuilder {
	t.Helper()
	reg := agent.NewRegistry()
	graph := &fakeEngineFactory{spec: graphSpec()}
	inline := &fakeEngineFactory{spec: agent.EngineSpec{Kind: "inline"}}
	for _, f := range []*fakeEngineFactory{graph, inline} {
		if err := reg.Register(f); err != nil {
			t.Fatalf("register %s: %v", f.spec.Kind, err)
		}
	}

	jr := &journal{}
	captured := &hookCapture{}
	b := deploy.NewBuilder(reg)

	b.RegisterSource("host.tools", func(_ context.Context, ref string) (any, error) {
		return "catalog:" + ref, nil
	})

	registryFactory := func(kind string) deploy.ResourceFunc {
		return func(_ context.Context, in deploy.ResourceInput) (any, error) {
			type s struct {
				Names []string `yaml:"names"`
			}
			dec, err := deploy.DecodeSettings[s](in.Settings)
			if err != nil {
				return nil, err
			}
			items := make(map[string]string, len(dec.Names))
			for _, n := range dec.Names {
				items[n] = kind + ":" + n
			}
			jr.built = append(jr.built, kind)
			return &fakeRegistry{name: kind, items: items, journal: jr}, nil
		}
	}
	b.RegisterResource("workspace.Registry", "fake", registryFactory("fs"))
	b.RegisterResource("sandbox.Registry", "fake", func(_ context.Context, in deploy.ResourceInput) (any, error) {
		// A resource binding another resource: the workspace registry
		// must already exist by the time this runs.
		dep, ok := in.Dep("workspaces")
		if !ok {
			return nil, errString("sandbox.Registry: workspaces dep is required")
		}
		if _, ok := dep.(*fakeRegistry); !ok {
			return nil, errString("sandbox.Registry: workspaces dep is not a registry")
		}
		return registryFactory("box")(context.Background(), in)
	})
	b.RegisterResource("script.runtime", "fakejs", func(_ context.Context, in deploy.ResourceInput) (any, error) {
		type s struct {
			PoolSize int `yaml:"pool_size"`
		}
		dec, err := deploy.DecodeSettings[s](in.Settings)
		if err != nil {
			return nil, err
		}
		jr.built = append(jr.built, "js_main")
		return &fakeJSRuntime{pool: dec.PoolSize, journal: jr}, nil
	})
	b.RegisterResource("fake.Store", "fake", func(_ context.Context, in deploy.ResourceInput) (any, error) {
		ws, ok := in.Dep("workspace")
		if !ok {
			return nil, errString("fake.Store: workspace dep is required")
		}
		jr.built = append(jr.built, "store")
		return &fakeStore{workspace: ws.(string)}, nil
	})

	b.RegisterObserver("fake_hook", func(_ context.Context, in deploy.HookInput) (agent.Observer, error) {
		type s struct {
			Store string `yaml:"store"`
		}
		if _, err := deploy.DecodeSettings[s](in.Settings); err != nil {
			return nil, err
		}
		h := &recordingHook{}
		if dep, ok := in.Dep("store"); ok {
			h.store, _ = dep.(*fakeStore)
		}
		captured.hook = h
		return h, nil
	})
	b.RegisterPreparer("fake_before", func(_ context.Context, in deploy.HookInput) (agent.Preparer, error) {
		type s struct {
			Window int `yaml:"window"`
		}
		dec, err := deploy.DecodeSettings[s](in.Settings)
		if err != nil {
			return nil, err
		}
		out := recordingBefore{window: dec.Window}
		if dep, ok := in.Dep("store"); ok {
			out.store, _ = dep.(*fakeStore)
		}
		captured.before = &out
		return out, nil
	})

	return &testBuilder{Builder: b, graph: graph, inline: inline, journal: jr, hooks: captured}
}

type errString string

func (e errString) Error() string { return string(e) }

func loadDoc(t *testing.T) deploy.Document {
	t.Helper()
	data, err := os.ReadFile("testdata/deploy.yaml")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	doc, err := deploy.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return doc
}

func parse(t *testing.T, body string) deploy.Document {
	t.Helper()
	doc, err := deploy.Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return doc
}

// ---------- Parse ----------

func TestParse_HappyPath(t *testing.T) {
	doc := loadDoc(t)
	if doc.Version != deploy.VersionV1 {
		t.Fatalf("Version = %q", doc.Version)
	}
	if len(doc.Agents) != 2 {
		t.Fatalf("Agents = %v, want 2 entries", doc.Agents)
	}
	if len(doc.Resources) != 4 {
		t.Fatalf("Resources = %v, want 4 entries", doc.Resources)
	}
	if got := doc.Resources["box"].Deps["workspaces"]; got.Resource != "fs" || got.Ref != "" {
		t.Errorf("Resources[box].deps[workspaces] = %+v", got)
	}
	if got := doc.Resources["store"].Deps["workspace"]; got.Resource != "fs" || got.Ref != "project" {
		t.Errorf("Resources[store].deps[workspace] = %+v", got)
	}

	r := doc.Agents["researcher"]
	if r.Engine.Kind != "graph" || r.Engine.Settings["max_steps"] != 8 {
		t.Errorf("Engine = %+v", r.Engine)
	}
	if len(r.Prepare) == 0 || r.Prepare[0].Type != "fake_before" ||
		r.Prepare[0].Deps["store"].Resource != "store" {
		t.Errorf("Prepare = %+v", r.Prepare)
	}
	if len(r.Observe) != 1 || r.Observe[0].Deps["store"].Resource != "store" {
		t.Errorf("Observe = %+v", r.Observe)
	}
}

// TestParse_DepRefScalarAndMapping pins the two dep spellings. A
// scalar always means a resource; a mapping is how a host-owned
// source is named, which is the visible half of the ownership rule.
func TestParse_DepRefScalarAndMapping(t *testing.T) {
	doc := parse(t, `
version: v1
agents:
  a:
    engine: {kind: graph}
    deps:
      whole: infer
      item: fs/project
      borrowed: {source: host.tools, ref: default}
`)
	deps := doc.Agents["a"].Deps
	if got := deps["whole"]; got.Resource != "infer" || got.Ref != "" || got.Source != "" {
		t.Errorf("whole = %+v", got)
	}
	if got := deps["item"]; got.Resource != "fs" || got.Ref != "project" {
		t.Errorf("item = %+v", got)
	}
	if got := deps["borrowed"]; got.Source != "host.tools" || got.Ref != "default" || got.Resource != "" {
		t.Errorf("borrowed = %+v", got)
	}
}

func TestParse_RejectsMalformedDepRef(t *testing.T) {
	for name, body := range map[string]string{
		"empty scalar":      "deps: {x: \"\"}",
		"leading slash":     "deps: {x: /project}",
		"trailing slash":    "deps: {x: fs/}",
		"two separators":    "deps: {x: a/b/c}",
		"both forms":        "deps: {x: {source: s, resource: r}}",
		"neither form":      "deps: {x: {ref: only}}",
		"unknown dep key":   "deps: {x: {resource: r, bogus: 1}}",
		"unknown agent key": "deps: {x: ok}\n    bogus: 1",
	} {
		body := "version: v1\nagents:\n  a:\n    engine: {kind: graph}\n    " + body + "\n"
		if _, err := deploy.Parse([]byte(body)); err == nil {
			t.Errorf("%s: Parse succeeded, want error", name)
		}
	}
}

func TestParse_RejectsSeparatorInResourceName(t *testing.T) {
	_, err := deploy.Parse([]byte(
		"version: v1\nresources:\n  a/b: {kind: k, impl: i}\nagents: {}\n"))
	if err == nil {
		t.Fatal("resource name containing the separator must fail Parse")
	}
}

func TestParse_StrictAndVersioned(t *testing.T) {
	if _, err := deploy.Parse([]byte("version: v2\nagents: {}\n")); err == nil {
		t.Error("unsupported version must fail")
	}
	if _, err := deploy.Parse([]byte("version: v1\nbogus: 1\nagents: {}\n")); err == nil {
		t.Error("unknown top-level field must fail")
	}
	if _, err := deploy.Parse([]byte("version: v1\nagents: {}\n---\nversion: v1\nagents: {}\n")); err == nil {
		t.Error("trailing document must fail")
	}
	if _, err := deploy.Parse([]byte(
		"version: v1\nresources:\n  a: {impl: i}\nagents: {}\n")); err == nil {
		t.Error("resource without kind must fail")
	}
}

// ---------- Build ----------

func TestBuild_AssemblesRunnableInstance(t *testing.T) {
	tb := newTestBuilder(t)
	res, err := tb.Build(context.Background(), loadDoc(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Instances) != 2 {
		t.Fatalf("instances = %v", res.Instances)
	}

	r := res.Instances["researcher"]
	if r.Agent.ID != "researcher" || r.Agent.Card.Name != "研究员" {
		t.Errorf("Agent = %+v", r.Agent)
	}

	// A container resource addressed by item yields the item, not the
	// container.
	if v, _ := tb.graph.gotCfg.Dep("workspace"); v != "fs:project" {
		t.Errorf("cfg.Dep(workspace) = %v, want fs:project", v)
	}
	if v, _ := tb.graph.gotCfg.Dep("runner"); v != "box:coding" {
		t.Errorf("cfg.Dep(runner) = %v, want box:coding", v)
	}
	// A whole-bound resource yields the instance itself.
	js, ok := tb.graph.gotCfg.Dep("script_runtime")
	if !ok {
		t.Fatal("cfg.Dep(script_runtime) missing")
	}
	if rt, ok := js.(*fakeJSRuntime); !ok || rt.pool != 8 {
		t.Errorf("script_runtime = %#v, want fakeJSRuntime{pool:8}", js)
	}
	if res.Resources["js_main"] != js {
		t.Error("Resources[js_main] should be the same instance the engine got")
	}
	// A source is borrowed, resolved through the host closure.
	if v, _ := tb.graph.gotCfg.Dep("tools"); v != "catalog:default" {
		t.Errorf("cfg.Dep(tools) = %v", v)
	}
	if v, _ := tb.graph.gotCfg.Setting("graph"); v != "graphs/research.json" {
		t.Errorf("cfg.Setting(graph) = %v", v)
	}

	execRes, err := r.Execute(context.Background(), agent.Request{
		Message: inference.NewTextMessage(inference.RoleUser, "hi"),
	})
	if err != nil {
		t.Fatalf("instance Execute: %v", err)
	}
	if execRes.Status != agent.StatusCompleted {
		t.Errorf("Status = %q", execRes.Status)
	}

	if _, err := res.Instances["minimal"].Execute(context.Background(), agent.Request{
		Message: inference.NewTextMessage(inference.RoleUser, "hi"),
	}); err != nil {
		t.Fatalf("minimal Execute: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestBuild_ResourceDepsBuildInTopologicalOrder is the regression test
// for ordering. "box" depends on "fs" but sorts before it, so a
// lexical build order would hand the sandbox factory a missing dep.
func TestBuild_ResourceDepsBuildInTopologicalOrder(t *testing.T) {
	tb := newTestBuilder(t)
	res, err := tb.Build(context.Background(), loadDoc(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	fsAt, boxAt, storeAt := -1, -1, -1
	for i, name := range tb.journal.built {
		switch name {
		case "fs":
			fsAt = i
		case "box":
			boxAt = i
		case "store":
			storeAt = i
		}
	}
	if fsAt < 0 || boxAt < 0 || storeAt < 0 {
		t.Fatalf("built = %v, want fs, box and store", tb.journal.built)
	}
	if fsAt > boxAt {
		t.Errorf("built = %v: fs must precede box", tb.journal.built)
	}
	if fsAt > storeAt {
		t.Errorf("built = %v: fs must precede store", tb.journal.built)
	}
}

// TestBuild_CloseReversesConstructionOrder: a resource must never
// outlive something it depends on, so close runs backwards.
func TestBuild_CloseReversesConstructionOrder(t *testing.T) {
	tb := newTestBuilder(t)
	res, err := tb.Build(context.Background(), loadDoc(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fsAt, boxAt := -1, -1
	for i, name := range tb.journal.closed {
		switch name {
		case "fs":
			fsAt = i
		case "box":
			boxAt = i
		}
	}
	if fsAt < 0 || boxAt < 0 {
		t.Fatalf("closed = %v, want fs and box", tb.journal.closed)
	}
	if boxAt > fsAt {
		t.Errorf("closed = %v: box must close before fs", tb.journal.closed)
	}
}

func TestBuild_ResourceCycleFails(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  a: {kind: workspace.Registry, impl: fake, deps: {peer: b}}
  b: {kind: workspace.Registry, impl: fake, deps: {peer: a}}
agents: {}
`)
	_, err := tb.Build(context.Background(), doc)
	if err == nil {
		t.Fatal("dependency cycle must fail Build")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %v, want it to mention a cycle", err)
	}
}

func TestBuild_ResourceDepOnUndefinedResourceFails(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  a: {kind: workspace.Registry, impl: fake, deps: {peer: ghost}}
agents: {}
`)
	if _, err := tb.Build(context.Background(), doc); err == nil {
		t.Fatal("resource dep on an undefined resource must fail Build")
	}
}

// TestBuild_HookDepsReachResources is what makes the resource area
// useful beyond engines: persistence / observer wiring lives in
// the hook layer, so hooks must be able to bind resources too.
func TestBuild_HookDepsReachResources(t *testing.T) {
	tb := newTestBuilder(t)
	res, err := tb.Build(context.Background(), loadDoc(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	if tb.hooks.before == nil || tb.hooks.before.store == nil {
		t.Fatal("before factory did not receive the store resource")
	}
	if tb.hooks.before.window != 20 {
		t.Errorf("before.window = %d, want 20", tb.hooks.before.window)
	}
	if tb.hooks.hook == nil || tb.hooks.hook.store == nil {
		t.Fatal("hook factory did not receive the store resource")
	}
	// The hook's store is the same instance the resource area built,
	// and it resolved its own workspace item dep.
	if tb.hooks.hook.store != res.Resources["store"] {
		t.Error("hook store should be the resource-area instance")
	}
	if got := res.Resources["store"].(*fakeStore).workspace; got != "fs:project" {
		t.Errorf("store.workspace = %q, want fs:project", got)
	}
}

// TestBuild_ResourceBoundOnlyByHookCountsAsUsed: the dead-config rule
// must count every consumer, not only agent deps, or a store bound
// exclusively by a hook would be rejected.
func TestBuild_ResourceBoundOnlyByHookCountsAsUsed(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  fs: {kind: workspace.Registry, impl: fake, settings: {names: [project]}}
  store: {kind: fake.Store, impl: fake, deps: {workspace: fs/project}}
agents:
  a:
    engine: {kind: inline}
    observe:
      - {type: fake_hook, deps: {store: store}}
`)
	res, err := tb.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })
}

func TestBuild_UnusedResourceFails(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  orphan: {kind: script.runtime, impl: fakejs, settings: {pool_size: 1}}
agents:
  a:
    engine: {kind: inline}
`)
	if _, err := tb.Build(context.Background(), doc); err == nil {
		t.Fatal("a resource nothing binds must fail Build")
	}
}

// TestBuild_ItemRefOnNonContainerFails: an inference runtime or tool
// assembly is a single object; addressing an item inside one is a
// wiring mistake and must be caught at build time.
func TestBuild_ItemRefOnNonContainerFails(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  js_main: {kind: script.runtime, impl: fakejs, settings: {pool_size: 1}}
agents:
  a:
    engine: {kind: graph}
    deps:
      workspace: js_main/nope
`)
	_, err := tb.Build(context.Background(), doc)
	if err == nil {
		t.Fatal("item ref on a non-container resource must fail Build")
	}
	if !strings.Contains(err.Error(), "not a container") {
		t.Errorf("error = %v, want it to say the resource is not a container", err)
	}
}

func TestBuild_UnknownItemInContainerFails(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  fs: {kind: workspace.Registry, impl: fake, settings: {names: [project]}}
agents:
  a:
    engine: {kind: graph}
    deps:
      workspace: fs/ghost
`)
	if _, err := tb.Build(context.Background(), doc); err == nil {
		t.Fatal("unknown item inside a container must fail Build")
	}
}

// TestBuild_WholeBindingChecksKindAgainstDepSpec: whole binding is the
// only form where the declared category is verifiable, so it must be
// verified.
func TestBuild_WholeBindingChecksKindAgainstDepSpec(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  js_main: {kind: script.runtime, impl: fakejs, settings: {pool_size: 1}}
agents:
  a:
    engine: {kind: graph}
    deps:
      workspace: js_main
`)
	_, err := tb.Build(context.Background(), doc)
	if err == nil {
		t.Fatal("kind/DepSpec.Type mismatch must fail Build")
	}
	if !strings.Contains(err.Error(), "dep expects") {
		t.Errorf("error = %v, want a kind mismatch message", err)
	}
}

func TestBuild_FailureClosesAlreadyBuiltResources(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  fs: {kind: workspace.Registry, impl: fake, settings: {names: [project]}}
agents:
  a:
    engine: {kind: ghost}
    deps:
      workspace: fs/project
`)
	if _, err := tb.Build(context.Background(), doc); err == nil {
		t.Fatal("unregistered engine kind must fail Build")
	}
	found := false
	for _, name := range tb.journal.closed {
		if name == "fs" {
			found = true
		}
	}
	if !found {
		t.Errorf("closed = %v, want fs closed after the failed build", tb.journal.closed)
	}
}

func TestBuild_UnknownResourceKindImplFails(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  x: {kind: workspace.Registry, impl: nope}
agents: {}
`)
	if _, err := tb.Build(context.Background(), doc); err == nil {
		t.Fatal("unregistered (kind, impl) must fail Build")
	}
}

func TestBuild_DepValidation(t *testing.T) {
	tb := newTestBuilder(t)

	for name, body := range map[string]string{
		"undeclared dep": `
version: v1
resources:
  fs: {kind: workspace.Registry, impl: fake, settings: {names: [project]}}
agents:
  a:
    engine: {kind: graph}
    deps: {workspace: fs/project, ghost: fs/project}
`,
		"missing required dep": `
version: v1
agents:
  a:
    engine: {kind: graph}
    deps: {tools: {source: host.tools}}
`,
		"unknown source": `
version: v1
agents:
  a:
    engine: {kind: graph}
    deps: {workspace: {source: nowhere}}
`,
		"undefined resource": `
version: v1
agents:
  a:
    engine: {kind: graph}
    deps: {workspace: ghost/x}
`,
	} {
		if _, err := tb.Build(context.Background(), parse(t, body)); err == nil {
			t.Errorf("%s: Build succeeded, want error", name)
		}
	}
}

func TestBuild_UnknownHookTypeFails(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, "version: v1\nagents:\n  a:\n    engine: {kind: inline}\n    observe: [{type: ghost}]\n")
	if _, err := tb.Build(context.Background(), doc); err == nil {
		t.Fatal("unregistered hook type must fail Build")
	}
}

func TestBuild_BuiltinDiscardOnInterrupt(t *testing.T) {
	tb := newTestBuilder(t)
	res, err := tb.Build(context.Background(), loadDoc(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	intr := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		b.AppendChannelMessage(agent.MainChannel, inference.NewTextMessage(inference.RoleAssistant, "partial"))
		return b, agent.Interrupted(agent.Interrupt{Cause: agent.CauseUserInput})
	})
	inst := res.Instances["researcher"]
	inst.Engine = intr

	execRes, err := inst.Execute(context.Background(), agent.Request{
		Message: inference.NewTextMessage(inference.RoleUser, "hi"),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if execRes.Status != agent.StatusInterrupted {
		t.Fatalf("Status = %q", execRes.Status)
	}
	if execRes.Committed {
		t.Error("builtin discard_on_interrupt must set Committed=false on user_input")
	}
}

func TestDecodeSettings_StrictAndNilSafe(t *testing.T) {
	type s struct {
		Window int `yaml:"window"`
	}
	if v, err := deploy.DecodeSettings[s](nil); err != nil || v.Window != 0 {
		t.Fatalf("nil node: (%v, %v)", v, err)
	}

	var node yamlv3.Node
	if err := yamlv3.Unmarshal([]byte("window: 3\n"), &node); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v, err := deploy.DecodeSettings[s](&node)
	if err != nil || v.Window != 3 {
		t.Fatalf("known field: (%v, %v)", v, err)
	}

	if err := yamlv3.Unmarshal([]byte("windo: 3\n"), &node); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, err := deploy.DecodeSettings[s](&node); err == nil {
		t.Fatal("typo key must fail strict decode")
	}
}
